import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { afterEach, describe, expect, it } from "vitest";
import {
  AIError,
  AnswerError,
  Client,
  DeepInfra,
  MIN_REASONING_MAX_TOKENS,
  buildPrompt,
  estimateTokens,
  isAIError,
  isReasoningModel,
  type Turn,
} from "./index";

type Handler = (req: Record<string, unknown>) => [number, string] | Promise<[number, string]>;

let server: Server | null = null;
const requests: Record<string, unknown>[] = [];

async function fakeDeepInfra(handler: Handler): Promise<string> {
  requests.length = 0;
  server = createServer(async (req: IncomingMessage, res: ServerResponse) => {
    let body = "";
    for await (const chunk of req) body += chunk;
    if (req.url !== "/v1/openai/chat/completions" || req.headers.authorization !== "Bearer test-key") {
      res.writeHead(401).end("bad path or auth");
      return;
    }
    const parsed = JSON.parse(body) as Record<string, unknown>;
    requests.push(parsed);
    const [code, payload] = await handler(parsed);
    res.writeHead(code).end(payload);
  });
  await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1/openai`;
}

afterEach(() => {
  server?.closeAllConnections();
  server?.close();
  server = null;
});

function completion(content: string, reasoning: number): string {
  return JSON.stringify({
    model: "x",
    choices: [{ message: { role: "assistant", content }, finish_reason: content ? "stop" : "length" }],
    usage: { prompt_tokens: 120, completion_tokens: 300, completion_tokens_details: { reasoning_tokens: reasoning } },
  });
}

const client = (url: string, primary: string, fallback: string, maxTokens: number) =>
  new Client(new DeepInfra(url, "test-key"), { primary, fallback, reasoningEffort: "low", maxOutputTokens: maxTokens, temperature: 0.2 });

async function failure(p: Promise<unknown>): Promise<AnswerError> {
  try {
    await p;
  } catch (err) {
    if (err instanceof AnswerError) return err;
    throw err;
  }
  throw new Error("expected a failure");
}

describe("ai client", () => {
  it("applies the reasoning guard only to reasoning models", async () => {
    const url = await fakeDeepInfra(() => [200, completion("Ohm's law: V = I x R.", 40)]);
    for (const model of ["openai/gpt-oss-20b", "meta-llama/Llama-3.3-70B-Instruct-Turbo"]) {
      await client(url, model, "", 100).answer([{ role: "user", content: "Ohm?" }]);
    }
    const [gpt, llama] = requests;
    expect(gpt!.reasoning_effort).toBe("low");
    expect(gpt!.max_tokens).toBeGreaterThanOrEqual(MIN_REASONING_MAX_TOKENS);
    expect(llama).not.toHaveProperty("reasoning_effort");
    expect(llama!.max_tokens).toBe(100);
  });

  it("falls back after an empty reasoning response and records its cost", async () => {
    const url = await fakeDeepInfra((req) =>
      req.model === "openai/gpt-oss-20b" ? [200, completion("", 350)] : [200, completion("Photosynthesis makes food from light.", 0)],
    );
    const out = await client(url, "openai/gpt-oss-20b", "meta-llama/Llama-3.3-70B-Instruct-Turbo", 300).answer([
      { role: "user", content: "photosynthesis?" },
    ]);
    expect(out.text).toBe("Photosynthesis makes food from light.");
    expect(out.calls).toHaveLength(2);
    expect(out.calls[0]!.status).toBe("empty");
    expect(out.calls[0]!.reasoningTokens).toBe(350);
    expect(out.calls[0]!.costUsd).toBeGreaterThan(0);
  });

  it("maps quota errors, skips the fallback and opens the breaker", async () => {
    const url = await fakeDeepInfra(() => [402, `{"error":"insufficient balance"}`]);
    const c = client(url, "meta-llama/Llama-3.3-70B-Instruct-Turbo", "openai/gpt-oss-20b", 300);
    const err = await failure(c.answer([{ role: "user", content: "hi" }]));
    expect(isAIError(err.error, "quota")).toBe(true);
    expect(requests).toHaveLength(1);
    const again = await failure(c.answer([]));
    expect(isAIError(again.error, "circuit_open")).toBe(true);
    expect(c.breaker.state()).toMatch(/^open/);
  });

  it("does not trip the breaker on caller deadlines", async () => {
    const url = await fakeDeepInfra(async () => {
      await new Promise((r) => setTimeout(r, 200));
      return [200, completion("late", 0)];
    });
    const c = client(url, "meta-llama/Llama-3.3-70B-Instruct-Turbo", "", 300);
    for (let i = 0; i < 6; i++) {
      const err = await failure(c.answer([{ role: "user", content: "hi" }], AbortSignal.timeout(20)));
      expect(err.error).toBeInstanceOf(AIError);
      expect((err.error as AIError).code).toBe("timeout");
    }
    expect(c.breaker.state()).toBe("closed");
  });
});

describe("prompt", () => {
  it("trims history to the budget and keeps facts and the date", () => {
    const history: Turn[] = [];
    for (let i = 0; i < 20; i++) history.push({ role: "user", content: "q".repeat(200) }, { role: "assistant", content: "a".repeat(200) });
    const msgs = buildPrompt({
      question: "Why do they need sunlight?",
      history,
      maxInputTokens: 800,
      facts: ["NHIS: dial *929#."],
      now: new Date(Date.UTC(2026, 9, 17, 9)),
    });
    expect(msgs.reduce((n, m) => n + estimateTokens(m.content), 0)).toBeLessThanOrEqual(800);
    expect(msgs[0]!.role).toBe("system");
    expect(msgs[0]!.content).toContain("Verified Ghana Fact: NHIS");
    expect(msgs[0]!.content).toContain("17 October 2026");
    expect(msgs[1]!.role).toBe("user");
    expect(msgs.at(-1)!.content).toBe("Why do they need sunlight?");
  });

  it("detects reasoning models", () => {
    expect(isReasoningModel("openai/gpt-oss-120b")).toBe(true);
    expect(isReasoningModel("Qwen/Qwen2.5-72B-Instruct")).toBe(false);
  });
});
