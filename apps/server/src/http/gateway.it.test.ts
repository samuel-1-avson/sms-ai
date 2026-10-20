// The phone gateway protocol end to end over HTTP (Hono's in-process
// request), against PostgreSQL. Set SMSAI_TEST_DATABASE_URL.
import { randomUUID } from "node:crypto";
import { GatewayPaths, signedHeaders, type OutboundSMS } from "@smsai/shared";
import { afterEach, describe, expect, it } from "vitest";
import { createServer, type ServerHandle } from "./server";
import { TEST_DB, TestEnv, baseEnviron } from "../test/env";

const SECRET = "gateway-test-secret-0123456789";
const OWN = "0559999999";
const phone = "0244123456";

let env: TestEnv | null = null;
let server: ServerHandle | null = null;

async function setup(extra: Record<string, string> = {}) {
  env = await TestEnv.create("smsai_it_gateway", undefined, (url) => ({
    ...baseEnviron(url),
    PRIMARY_SMS_PROVIDER: "gateway",
    GATEWAY_DEVICE_SECRET: SECRET,
    GATEWAY_PHONE_NUMBER: OWN,
    ...extra,
  }));
  server = await createServer(env.app);
  return env;
}

afterEach(async () => {
  await server?.close();
  await env?.close();
  server = null;
  env = null;
});

async function post(path: string, body: unknown, opts: { secret?: string; ts?: number } = {}) {
  const text = JSON.stringify(body);
  const headers = signedHeaders(opts.secret ?? SECRET, text, opts.ts ?? Math.floor(Date.now() / 1000));
  const res = await server!.app.request(path, { method: "POST", headers, body: text });
  return { status: res.status, json: (await res.json()) as Record<string, unknown> };
}

const inbound = (from: string, body: string, id = randomUUID()) => ({
  messages: [{ id, from, body, received_at: new Date().toISOString() }],
});

async function outbox(wait = 0): Promise<OutboundSMS[]> {
  const r = await post(GatewayPaths.outbox, { wait });
  expect(r.status).toBe(200);
  return r.json.messages as OutboundSMS[];
}

describe.skipIf(!TEST_DB)("gateway protocol (integration)", { timeout: 20_000 }, () => {
  it("runs the full loop: inbound, answer, outbox, ack, sent, delivered", async () => {
    const e = await setup();
    const r = await post(GatewayPaths.inbound, inbound("+233244123456", "What is photosynthesis?"));
    expect(r.status).toBe(200);
    expect(r.json.messages).toBe(1);
    await e.drain();

    const msgs = await outbox();
    expect(msgs).toHaveLength(1);
    expect(msgs[0]!.to).toBe("+233244123456");
    expect(msgs[0]!.body).toMatch(/^Photosynthesis/);
    expect(await e.value(`SELECT status FROM messages WHERE id = $1`, msgs[0]!.id)).toBe("submitted");

    // Unacknowledged rows are offered again; acknowledged ones are not.
    expect((await outbox()).map((m) => m.id)).toEqual([msgs[0]!.id]);
    expect((await post(GatewayPaths.ack, { ids: [msgs[0]!.id] })).json.acked).toBe(1);
    expect(await outbox()).toHaveLength(0);

    await post(GatewayPaths.status, { statuses: [{ id: msgs[0]!.id, status: "sent" }] });
    expect(await e.value(`SELECT state FROM gateway_outbox`)).toBe("sent");
    await post(GatewayPaths.status, { statuses: [{ id: msgs[0]!.id, status: "delivered" }] });
    expect(await e.value(`SELECT state FROM gateway_outbox`)).toBe("delivered");
    expect(await e.value(`SELECT status FROM messages WHERE id = $1`, msgs[0]!.id)).toBe("delivered");
    // A late "sent" never moves a delivered message back.
    await post(GatewayPaths.status, { statuses: [{ id: msgs[0]!.id, status: "sent" }] });
    expect(await e.value(`SELECT state FROM gateway_outbox`)).toBe("delivered");
    expect(await e.value(`SELECT status FROM messages WHERE id = $1`, msgs[0]!.id)).toBe("delivered");
  });

  it("rejects bad and stale signatures", async () => {
    await setup();
    expect((await post(GatewayPaths.inbound, inbound(phone, "hi"), { secret: "wrong-secret-0123456789abcdef" })).status).toBe(401);
    expect((await post(GatewayPaths.inbound, inbound(phone, "hi"), { ts: Math.floor(Date.now() / 1000) - 600 })).status).toBe(401);
    expect((await post(GatewayPaths.outbox, {}, { secret: "wrong-secret-0123456789abcdef" })).status).toBe(401);
    const unsigned = await server!.app.request(GatewayPaths.inbound, { method: "POST", body: JSON.stringify(inbound(phone, "hi")) });
    expect(unsigned.status).toBe(401);
    expect((await post(GatewayPaths.inbound, { messages: [] })).status).toBe(400);
  });

  it("accepts a rotated secret", async () => {
    await setup({ GATEWAY_DEVICE_SECRET: `new-secret-0123456789abcdefgh, ${SECRET}` });
    expect((await post(GatewayPaths.inbound, inbound(phone, "HELP"))).status).toBe(200);
    expect((await post(GatewayPaths.inbound, inbound(phone, "HELP"), { secret: "new-secret-0123456789abcdefgh" })).status).toBe(200);
  });

  it("treats a retried inbound batch as a duplicate", async () => {
    const e = await setup();
    const batch = inbound(phone, "What is photosynthesis?");
    expect((await post(GatewayPaths.inbound, batch)).json.duplicates).toBe(0);
    expect((await post(GatewayPaths.inbound, batch)).json.duplicates).toBe(1);
    await e.drain();
    expect(await e.count(`SELECT count(*) FROM jobs WHERE kind = 'answer'`)).toBe(1);
    expect(await outbox()).toHaveLength(1);
  });

  it("drops network promos, short codes and its own number without replying", async () => {
    const e = await setup();
    const r = await post(GatewayPaths.inbound, {
      messages: [
        { id: randomUUID(), from: "MTN", body: "Get 2GB for GHS5!", received_at: new Date().toISOString() },
        { id: randomUUID(), from: "1234", body: "Your balance is low", received_at: new Date().toISOString() },
        { id: randomUUID(), from: "+233559999999", body: "loop?", received_at: new Date().toISOString() },
      ],
    });
    expect(r.status).toBe(200);
    expect(r.json).toMatchObject({ messages: 0, dropped: 3 });
    await e.drain();
    expect(await e.count(`SELECT count(*) FROM messages`)).toBe(0);
    expect(await outbox()).toHaveLength(0);
  });

  it("wakes a long-poll as soon as a reply is queued", async () => {
    const e = await setup();
    const started = Date.now();
    const poll = outbox(10);
    await new Promise((r) => setTimeout(r, 200));
    await post(GatewayPaths.inbound, inbound(phone, "HELP"));
    await e.drain();
    const msgs = await poll;
    expect(msgs).toHaveLength(1);
    expect(msgs[0]!.body).toMatch(/^SMS AI Ghana: text any question/);
    expect(Date.now() - started).toBeLessThan(5000);
  });

  it("gives up replies the phone never collected and refunds their segments", async () => {
    const e = await setup();
    await post(GatewayPaths.inbound, inbound(phone, "HELP"));
    await e.drain();
    expect(await e.count(`SELECT sms_segments FROM global_usage_daily`)).toBe(1);
    await e.store.sql`UPDATE gateway_outbox SET created_at = now() - interval '7 hours'`;
    await e.engine.gatewayMaintenance();
    expect(await e.value(`SELECT state FROM gateway_outbox`)).toBe("expired");
    expect(await e.value(`SELECT status FROM messages WHERE direction = 'outbound'`)).toBe("failed");
    expect(await e.count(`SELECT sms_segments FROM global_usage_daily`)).toBe(0);
    expect(await outbox()).toHaveLength(0);
  });

  it("flags acknowledged replies with no status and never re-offers them", async () => {
    const e = await setup();
    await post(GatewayPaths.inbound, inbound(phone, "HELP"));
    await e.drain();
    const [m] = await outbox();
    await post(GatewayPaths.ack, { ids: [m!.id] });
    await e.store.sql`UPDATE gateway_outbox SET acked_at = now() - interval '1 hour'`;
    await e.engine.gatewayMaintenance();
    expect(await e.count(`SELECT count(*) FROM gateway_outbox WHERE stale_flagged`)).toBe(1);
    expect(await outbox()).toHaveLength(0);
  });

  it("records heartbeats and shows them on /admin/status", async () => {
    const adminToken = "admin-token-0123456789abcdef";
    await setup({ ADMIN_API_TOKEN: adminToken });
    const r = await post(GatewayPaths.heartbeat, {
      device_id: "pixel-1",
      app_version: "0.1.0",
      battery: 81,
      charging: true,
      signal: 3,
      sim_label: "MTN",
      inbox_pending: 0,
      outbox_pending: 2,
      status_pending: 1,
      paused: false,
    });
    expect(r.status).toBe(200);
    expect(r.json.sms_outbound_enabled).toBe(true);
    expect((await server!.app.request("/admin/status")).status).toBe(401);
    const res = await server!.app.request("/admin/status", { headers: { Authorization: `Bearer ${adminToken}` } });
    const status = (await res.json()) as { gateway: { devices: { device_id: string; outbox_pending: number }[] } };
    expect(status.gateway.devices[0]).toMatchObject({ device_id: "pixel-1", outbox_pending: 2 });
  });
});
