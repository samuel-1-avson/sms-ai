// Integration tests for the plan v2.0 §16 acceptance checklist (minus
// USSD). They need PostgreSQL: set SMSAI_TEST_DATABASE_URL.
import { afterEach, describe, expect, it } from "vitest";
import { AIError, MIN_REASONING_MAX_TOKENS } from "../ai";
import { analyze, segmentCount } from "../formatting";
import { Flag } from "../repository/queries";
import { TEST_DB, TEST_ENCRYPTION_KEY, TEST_HMAC_KEY, TestEnv, baseEnviron, derivedKey, freshDatabase, keyEnviron } from "../test/env";
import type { Config } from "../config";

const phone = "0244123456";
let envs: TestEnv[] = [];

async function newEnv(mutate?: (c: Config) => void): Promise<TestEnv> {
  const env = await TestEnv.create("smsai_it_service", mutate);
  envs.push(env);
  return env;
}

afterEach(async () => {
  for (const e of envs) await e.close();
  envs = [];
});

describe.skipIf(!TEST_DB)("service (integration)", { timeout: 20_000 }, () => {
  it("answers a standard question in one sanitized GSM-7 segment", async () => {
    const env = await newEnv();
    const res = await env.sms(phone, "What is photosynthesis?");
    expect(res.jobId).toBeTruthy();
    await env.drain();
    const body = env.lastSent();
    const st = analyze(body);
    expect(st.encoding).toBe("gsm7");
    expect(st.segments).toBe(1);
    // The mock model answers with "Sure!", an em dash and a curly apostrophe.
    expect(body.startsWith("Sure")).toBe(false);
    expect(body).not.toMatch(/[—’]/);
    expect(env.gw.sent[0]!.to).toBe("+233244123456");
  });

  it("passes context to a follow-up", async () => {
    const env = await newEnv();
    await env.sms(phone, "What is photosynthesis?");
    await env.drain();
    await env.sms(phone, "Why do they need sunlight?");
    await env.drain();
    expect(env.lastSent()).toMatch(/^Sunlight supplies the energy/);
    const msgs = env.model.lastRequest!.messages;
    const history = msgs.slice(1, -1).map((m) => `${m.role}: ${m.content}`).join(" | ");
    expect(history).toContain("user: What is photosynthesis?");
    expect(history).toContain("assistant: Photosynthesis");
  });

  it("coalesces split messages into one model call", async () => {
    const env = await newEnv();
    const first = await env.sms(phone, "Solve 2x + 5 = 15");
    await new Promise((r) => setTimeout(r, 100));
    const second = await env.sms(phone, "and explain each step");
    expect(second.coalesced).toBe(true);
    expect(second.jobId).toBe(first.jobId);
    await env.drain();
    expect(env.model.calls).toBe(1);
    expect(env.gw.sent).toHaveLength(1);
    expect(env.model.lastRequest!.messages.at(-1)!.content).toBe("Solve 2x + 5 = 15 and explain each step");
  });

  it("ignores duplicate webhooks", async () => {
    const env = await newEnv();
    const a = await env.sms(phone, "What is photosynthesis?", "provider-msg-1");
    const b = await env.sms(phone, "What is photosynthesis?", "provider-msg-1");
    expect(a.duplicate).toBe(false);
    expect(b.duplicate).toBe(true);
    await env.drain();
    expect(await env.count(`SELECT count(*) FROM jobs WHERE kind = 'answer'`)).toBe(1);
    expect(env.gw.sent).toHaveLength(1);
  });

  it("serves MORE from the buffer with zero LLM calls", async () => {
    const env = await newEnv();
    await env.sms(phone, "Give me a long explanation of rainfall");
    await env.drain();
    const part1 = env.lastSent();
    expect(part1.endsWith("..Txt MORE")).toBe(true);
    expect(segmentCount(part1)).toBe(1);
    const calls = env.model.calls;
    await env.sms(phone, "MORE");
    await env.drain();
    expect(env.model.calls).toBe(calls);
    const part2 = env.lastSent();
    expect(part2).not.toBe(part1);
    expect(segmentCount(part2)).toBe(1);
    await env.sms(phone, "MORE");
    await env.drain();
    expect(env.lastSent()).toMatch(/^Nothing more/);
  });

  it("answers grounded questions without the model", async () => {
    const env = await newEnv();
    await env.sms(phone, "How do I check my NHIS renewal?");
    await env.sms("0201234567", "How to link Ghana Card to MTN?");
    await env.drain();
    expect(env.model.calls).toBe(0);
    const bodies = env.sentBodies().join("\n");
    expect(bodies).toContain("*929#");
    expect(bodies).toContain("Ghana Card to your SIM");
    expect(await env.count(`SELECT count(*) FROM messages WHERE answer_source = 'grounding'`)).toBe(2);
  });

  it("falls back when a reasoning model returns nothing, and records the empty call", async () => {
    const env = await newEnv((c) => {
      c.llmModel = "openai/gpt-oss-20b";
      c.llmFallbackModel = "meta-llama/Llama-3.3-70B-Instruct-Turbo";
    });
    const seen: string[] = [];
    env.model.respond = (req) => {
      seen.push(req.model);
      if (req.model === "openai/gpt-oss-20b") {
        expect(req.reasoningEffort).toBe("low");
        expect(req.maxOutputTokens).toBeGreaterThanOrEqual(MIN_REASONING_MAX_TOKENS);
        throw new AIError("empty", "empty response");
      }
      return "Fallback answer from the instruct model.";
    };
    await env.sms(phone, "What is a noun?");
    await env.drain();
    expect(seen).toEqual(["openai/gpt-oss-20b", "meta-llama/Llama-3.3-70B-Instruct-Turbo"]);
    expect(env.lastSent()).toBe("Fallback answer from the instruct model.");
    expect(await env.count(`SELECT count(*) FROM model_calls WHERE status = 'empty'`)).toBe(1);
  });

  it("enforces the daily limit, then unlocks questions with a voucher", async () => {
    const env = await newEnv((c) => (c.dailyPerNumberFreeCap = 2));
    for (const q of ["What is a verb?", "What is an adverb?"]) {
      await env.sms(phone, q);
      await env.drain();
    }
    expect((await env.sms(phone, "What is a pronoun?")).dropped).toBe("over_limit");
    await env.sms(phone, "What is a conjunction?");
    await env.drain();
    expect(env.sentBodies().filter((b) => b.startsWith("Daily limit reached (2/2)"))).toHaveLength(1);

    await env.store.q().createPool({ voucher_code: "WASSCE2026", sponsor_name: "Test NGO", max_segments: 50, per_user_grant: 5 });
    await env.sms(phone, "CODE WASSCE2026");
    await env.drain();
    expect(env.lastSent()).toMatch(/^Code accepted: 5 extra questions/);
    await env.sms(phone, "CODE WASSCE2026");
    await env.drain();
    expect(env.lastSent()).toContain("already used");

    const res = await env.sms(phone, "What is a pronoun?");
    expect(res.jobId).toBeTruthy();
    await env.drain();
    expect(await env.count(`SELECT bonus_sms_quota FROM users`)).toBe(4);
  });

  it("finds users across a key rotation and re-keys them", async () => {
    const dbUrl = await freshDatabase("smsai_it_rotation");
    const v1: [string, string] = [TEST_ENCRYPTION_KEY, TEST_HMAC_KEY];
    const v2: [string, string] = [derivedKey(100), derivedKey(200)];

    const envV1 = await TestEnv.on(dbUrl, keyEnviron(baseEnviron(dbUrl), 1, { 1: v1 }));
    await envV1.sms(phone, "What is photosynthesis?");
    await envV1.drain();
    const userId = await envV1.value(`SELECT id::text FROM users`);
    await envV1.close();

    const envV2 = await TestEnv.on(dbUrl, keyEnviron(baseEnviron(dbUrl), 2, { 1: v1, 2: v2 }));
    envs.push(envV2);
    await envV2.sms(phone, "Why do they need sunlight?");
    await envV2.drain();
    expect(await envV2.count(`SELECT count(*) FROM users`)).toBe(1);
    expect(await envV2.count(`SELECT count(*) FROM users WHERE id = $1 AND key_version = 2`, userId)).toBe(1);
    expect(envV2.lastSent()).toMatch(/^Sunlight supplies/);
    const st = await envV2.store.rekey(100);
    expect(st.messages).toBeGreaterThan(0);
    expect(await envV2.count(`SELECT count(*) FROM messages WHERE key_version <> 2`)).toBe(0);
  });

  it("handles STOP and START", async () => {
    const env = await newEnv();
    await env.sms(phone, "STOP");
    await env.drain();
    expect(env.lastSent()).toContain("no more messages");
    expect((await env.sms(phone, "What is photosynthesis?")).dropped).toBe("opted_out");
    await env.sms(phone, "START");
    await env.drain();
    expect(env.lastSent()).toMatch(/^Welcome/);
    expect((await env.sms(phone, "What is photosynthesis?")).jobId).toBeTruthy();
  });

  it("reconciles uncertain sends instead of resending them", async () => {
    const env = await newEnv();
    // Case 1: the provider accepted the message but the reply was lost.
    env.gw.failWith("uncertain");
    await env.sms(phone, "HELP");
    await env.drain();
    const msgId = await env.value(`SELECT id::text FROM messages WHERE direction = 'outbound' AND status = 'unknown_timeout'`);
    expect(msgId).toBeTruthy();
    env.gw.recordAcceptedWithoutReply(msgId);
    await env.engine.reconcileUncertainSends();
    await env.drain();
    expect(await env.count(`SELECT count(*) FROM messages WHERE id = $1 AND status = 'submitted'`, msgId)).toBe(1);
    expect(env.gw.sent).toHaveLength(0);

    // Case 2: the provider never saw it: re-queue and send once.
    env.gw.failWith("uncertain");
    await env.sms(phone, "NEW");
    await env.drain();
    await env.engine.reconcileUncertainSends();
    await env.drain();
    expect(env.sentBodies()).toEqual(["New chat started. Ask me anything."]);
  });

  it("keeps delivery receipts monotonic", async () => {
    const env = await newEnv();
    await env.sms(phone, "HELP");
    await env.drain();
    const id = env.gw.sent[0]!.providerMessageId;
    for (const status of ["delivered", "submitted", "delivered"]) {
      await env.engine.handleDeliveryReceipts([{ provider: "mock", providerMessageId: id, status, at: new Date() }]);
    }
    expect(await env.count(`SELECT count(*) FROM messages WHERE provider_message_id = $1 AND status = 'delivered'`, id)).toBe(1);
  });

  it("blocks sends past the global segment cap", async () => {
    const env = await newEnv((c) => (c.globalDailySmsSegmentCap = 1));
    await env.sms(phone, "HELP");
    await env.sms("0201234567", "HELP");
    await env.drain();
    expect(env.gw.sent).toHaveLength(1);
    expect(await env.count(`SELECT count(*) FROM messages WHERE status = 'blocked_cap'`)).toBe(1);
  });

  it("sends one busy notice and refunds when the AI kill switch is off", async () => {
    const env = await newEnv();
    await env.store.q().setFlag(Flag.aiEnabled, false);
    env.engine.invalidateFlags();
    await env.sms(phone, "What is a verb?");
    await env.drain();
    await env.sms(phone, "What is a noun?");
    await env.drain();
    expect(env.model.calls).toBe(0);
    expect(env.sentBodies()).toHaveLength(1);
    expect(env.sentBodies()[0]).toContain("busy");
    expect(await env.count(`SELECT questions_used FROM usage_daily`)).toBe(0);
    await env.sms(phone, "How do I check my NHIS renewal?");
    await env.drain();
    expect(env.lastSent()).toContain("*929#");
  });

  it("deletes history on DELETE", async () => {
    const env = await newEnv();
    await env.sms(phone, "What is photosynthesis?");
    await env.drain();
    await env.sms(phone, "DELETE");
    await env.drain();
    expect(await env.count(`SELECT count(*) FROM messages WHERE kind IN ('question', 'answer')`)).toBe(0);
    expect(env.lastSent()).toContain("deleted");
  });

  it("scrubs PII before storage and the model", async () => {
    const env = await newEnv();
    await env.sms(phone, "My MoMo PIN is 4321 and my card GHA-123456789-0 is not working, help");
    await env.drain();
    const q = env.model.lastRequest!.messages.at(-1)!.content;
    expect(q).not.toContain("4321");
    expect(q).not.toContain("GHA-123456789-0");
    const convId = await env.value(`SELECT id::text FROM conversations LIMIT 1`);
    for (const turn of await env.store.q().recentTurns(convId, "", new Date(0), 10)) expect(turn.content).not.toContain("4321");
  });
});
