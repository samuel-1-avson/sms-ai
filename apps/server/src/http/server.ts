// HTTP routes: health, metrics, SMS webhooks, the phone gateway protocol,
// the admin API and (in development) the simulator (plan §9).
import { EventEmitter } from "node:events";
import { readFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { AckRequest, GatewayPaths, Heartbeat, MAX_BATCH, OutboxRequest, constantTimeEqual } from "@smsai/shared";
import { Hono, type Context } from "hono";
import { z } from "zod";
import type { App } from "../app";
import { BadPayloadError, BadSignatureError, type RawRequest } from "../channel/channel";
import { GATEWAY, parseBody } from "../channel/gateway";
import { isDevelopment } from "../config";
import { tryNormalize } from "../msisdn";
import { resolveAsset } from "../paths";
import { NotFoundError } from "../repository/queries";
import { GATEWAY_CHANNEL, dayOf } from "../repository/store";

const MAX_BODY = 64 * 1024;

export interface ServerHandle {
  app: Hono;
  /** Stops the gateway long-poll listener. */
  close(): Promise<void>;
}

async function rawRequest(c: Context): Promise<RawRequest> {
  const len = Number(c.req.header("content-length") ?? "0");
  if (len > MAX_BODY) throw new BadPayloadError("body too large");
  const body = await c.req.text();
  if (Buffer.byteLength(body) > MAX_BODY) throw new BadPayloadError("body too large");
  return { header: (n) => c.req.header(n), body };
}

function bearer(c: Context): string {
  const h = c.req.header("authorization") ?? "";
  return h.startsWith("Bearer ") ? h.slice(7).trim() : "";
}

export async function createServer(a: App): Promise<ServerHandle> {
  const app = new Hono();
  const { cfg, log, store, engine, metrics } = a;

  // Wakes long-polling phones as soon as a reply is queued (from any process).
  const outboxEvents = new EventEmitter().setMaxListeners(100);
  const sub = a.gateway ? await store.listen(GATEWAY_CHANNEL, () => outboxEvents.emit("ready")) : null;

  const reject = (c: Context, provider: string, err: unknown) => {
    if (err instanceof BadSignatureError) {
      metrics.webhookRejected.inc({ provider, reason: "signature" });
      log.warn({ provider }, "webhook rejected: bad signature");
      return c.json({ error: "invalid signature" }, 401);
    }
    if (err instanceof BadPayloadError) {
      metrics.webhookRejected.inc({ provider, reason: "payload" });
      return c.json({ error: err.message }, 400);
    }
    log.error({ provider, err }, "webhook processing failed");
    return c.json({ error: "temporary failure; please retry" }, 500);
  };

  app.use("*", async (c, next) => {
    const start = performance.now();
    await next();
    c.header("X-Content-Type-Options", "nosniff");
    const p = c.req.path;
    if (p === "/healthz" || p === "/metrics" || p.startsWith("/admin/simulate/outbox") || p === GatewayPaths.outbox) return;
    // Method, route, status and latency only: never bodies or phone numbers.
    log.info({ method: c.req.method, path: p, status: c.res.status, ms: Math.round(performance.now() - start) }, "http");
  });

  app.onError((err, c) => {
    log.error({ path: c.req.path, err }, "unhandled error in handler");
    return c.json({ error: "internal error" }, 500);
  });

  // ---------------------------------------------------------------------------
  // Health and metrics
  // ---------------------------------------------------------------------------

  app.get("/healthz", (c) => c.json({ status: "ok" }));

  app.get("/readyz", async (c) => {
    try {
      await Promise.race([store.ping(), new Promise((_, rej) => setTimeout(() => rej(new Error("timeout")), 2000))]);
    } catch {
      return c.json({ status: "unavailable", reason: "database" }, 503);
    }
    return c.json({ status: "ready", grounding_snippets: engine.matcher.size, ai_breaker: a.ai.breaker.state() });
  });

  app.get("/metrics", async (c) => {
    if (cfg.metricsToken && !constantTimeEqual(bearer(c), cfg.metricsToken)) return c.json({ error: "metrics token required" }, 401);
    return c.text(await metrics.registry.metrics(), 200, { "Content-Type": metrics.registry.contentType });
  });

  // ---------------------------------------------------------------------------
  // Generic SMS webhooks (the mock gateway in development and tests)
  // ---------------------------------------------------------------------------

  const smsInbound = async (c: Context) => {
    const name = c.req.param("provider")!;
    const p = a.registry.smsInbound(name);
    if (!p || name === GATEWAY) return c.json({ error: "unknown provider" }, 404);
    try {
      const events = p.parseInbound(await rawRequest(c));
      let duplicates = 0;
      for (const ev of events) if ((await engine.handleInboundSMS(ev)).duplicate) duplicates++;
      return c.json({ status: "accepted", messages: events.length, duplicates });
    } catch (err) {
      return reject(c, name, err);
    }
  };
  const smsStatus = async (c: Context) => {
    const name = c.req.param("provider")!;
    const p = a.registry.smsInbound(name);
    if (!p || name === GATEWAY) return c.json({ error: "unknown provider" }, 404);
    try {
      const receipts = p.parseStatus(await rawRequest(c));
      await engine.handleDeliveryReceipts(receipts);
      return c.json({ status: "accepted", receipts: receipts.length });
    } catch (err) {
      return reject(c, name, err);
    }
  };
  app.post("/webhooks/sms/:provider/inbound", smsInbound);
  app.post("/webhooks/sms/:provider/status", smsStatus);

  // ---------------------------------------------------------------------------
  // The phone gateway protocol (all requests signed with the device secret)
  // ---------------------------------------------------------------------------

  if (a.gateway) {
    const gw = a.gateway;

    app.post(GatewayPaths.inbound, async (c) => {
      try {
        const events = gw.parseInbound(await rawRequest(c));
        for (const reason of gw.lastDropped) metrics.gatewayInboundDropped.inc({ reason });
        let duplicates = 0;
        for (const ev of events) if ((await engine.handleInboundSMS(ev)).duplicate) duplicates++;
        return c.json({ status: "accepted", messages: events.length, dropped: gw.lastDropped.length, duplicates });
      } catch (err) {
        return reject(c, GATEWAY, err);
      }
    });

    app.post(GatewayPaths.outbox, async (c) => {
      let wait: number;
      try {
        const req = await rawRequest(c);
        gw.authenticate(req);
        wait = parseBody(OutboxRequest, req.body || "{}").wait;
      } catch (err) {
        return reject(c, GATEWAY, err);
      }
      let messages = await store.q().gatewayOffer(MAX_BATCH);
      if (messages.length === 0 && wait > 0) {
        await new Promise<void>((resolve) => {
          const done = () => {
            clearTimeout(timer);
            outboxEvents.off("ready", done);
            c.req.raw.signal.removeEventListener("abort", done);
            resolve();
          };
          const timer = setTimeout(done, wait * 1000);
          outboxEvents.on("ready", done);
          c.req.raw.signal.addEventListener("abort", done);
        });
        if (!c.req.raw.signal.aborted) messages = await store.q().gatewayOffer(MAX_BATCH);
      }
      return c.json({ messages });
    });

    app.post(GatewayPaths.ack, async (c) => {
      try {
        const req = await rawRequest(c);
        gw.authenticate(req);
        const { ids } = parseBody(AckRequest, req.body);
        return c.json({ acked: await store.q().gatewayAck(ids) });
      } catch (err) {
        return reject(c, GATEWAY, err);
      }
    });

    app.post(GatewayPaths.status, async (c) => {
      try {
        const req = await rawRequest(c);
        const updates = gw.statusUpdates(req);
        let unknown = 0;
        for (const u of updates) if (!(await store.q().gatewayStatus(u.id, u.status, u.error))) unknown++;
        await engine.handleDeliveryReceipts(
          updates.map((u) => ({ provider: GATEWAY, providerMessageId: u.id, status: u.status, errorCode: u.error, at: new Date() })),
        );
        return c.json({ status: "accepted", statuses: updates.length, unknown });
      } catch (err) {
        return reject(c, GATEWAY, err);
      }
    });

    app.post(GatewayPaths.heartbeat, async (c) => {
      try {
        const req = await rawRequest(c);
        gw.authenticate(req);
        const h = parseBody(Heartbeat, req.body);
        await store.q().gatewayHeartbeat(h);
        metrics.gatewayLastSeen.set(Date.now() / 1000);
        metrics.gatewayQueue.set({ queue: "inbox" }, h.inbox_pending);
        metrics.gatewayQueue.set({ queue: "outbox" }, h.outbox_pending);
        metrics.gatewayQueue.set({ queue: "status" }, h.status_pending);
        if (h.battery !== null) metrics.gatewayBattery.set(h.battery);
        const flags = await store.q().flags();
        return c.json({ status: "ok", server_time: new Date().toISOString(), sms_outbound_enabled: flags.sms_outbound_enabled });
      } catch (err) {
        return reject(c, GATEWAY, err);
      }
    });
  }

  // ---------------------------------------------------------------------------
  // Admin (ADMIN_API_TOKEN; open in development when unset)
  // ---------------------------------------------------------------------------

  const admin = new Hono();
  admin.use("*", async (c, next) => {
    const token = cfg.adminApiToken;
    if (!token && isDevelopment(cfg)) return next();
    if (!token || !constantTimeEqual(bearer(c), token)) return c.json({ error: "admin token required" }, 401);
    return next();
  });

  admin.get("/usage", async (c) => {
    const day = c.req.query("day") ?? dayOf(new Date());
    if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return c.json({ error: "day must be YYYY-MM-DD" }, 400);
    return c.json(await store.q().usageReport(day, cfg.smsSegmentCostUsd, cfg.globalDailySpendCapUsd, cfg.globalDailySmsSegmentCap));
  });

  admin.get("/status", async (c) => {
    const q = store.q();
    const gateway = a.gateway ? { devices: await q.gatewayDevices(), outbox: await q.gatewayCounts() } : null;
    return c.json({
      env: cfg.env,
      sms_provider: cfg.primarySmsProvider,
      llm_provider: cfg.llmProvider,
      llm_model: cfg.llmModel,
      llm_fallback_model: cfg.llmFallbackModel,
      ai_breaker: a.ai.breaker.state(),
      flags: await q.flags(),
      grounding_snippets: engine.matcher.size,
      key_version: cfg.currentKeyVersion,
      insecure_dev_keys: cfg.insecureDevKeys,
      gateway,
    });
  });

  admin.get("/flags", async (c) => c.json(await store.q().flags()));
  admin.post("/flags", async (c) => {
    const body = await c.req.json().catch(() => null);
    const parsed = z.record(z.boolean()).safeParse(body);
    if (!parsed.success || Object.keys(parsed.data).length === 0) return c.json({ error: 'body must be like {"ai_enabled": false}' }, 400);
    for (const [k, v] of Object.entries(parsed.data)) {
      try {
        await store.q().setFlag(k, v);
      } catch (err) {
        return c.json({ error: (err as Error).message }, 400);
      }
      log.warn({ flag: k, value: v }, "kill switch changed");
    }
    engine.invalidateFlags();
    return c.json(await store.q().flags());
  });

  admin.get("/pools", async (c) => c.json({ pools: await store.q().listPools() }));
  admin.post("/pools", async (c) => {
    const parsed = z
      .object({
        voucher_code: z.string().trim().toUpperCase().min(3).max(24),
        sponsor_name: z.string().min(1),
        max_segments: z.number().int().positive(),
        per_user_grant: z.number().int().positive(),
        expires_at: z.coerce.date().nullish(),
      })
      .safeParse(await c.req.json().catch(() => null));
    if (!parsed.success) return c.json({ error: "need voucher_code (3-24 chars), sponsor_name, max_segments > 0, per_user_grant > 0" }, 400);
    try {
      return c.json(await store.q().createPool(parsed.data), 201);
    } catch {
      return c.json({ error: "could not create pool (duplicate code?)" }, 409);
    }
  });
  admin.post("/pools/:code/deactivate", async (c) => {
    try {
      await store.q().setPoolActive(c.req.param("code"), false);
      return c.json({ status: "deactivated" });
    } catch (err) {
      if (err instanceof NotFoundError) return c.json({ error: "pool not found" }, 404);
      throw err;
    }
  });

  admin.get("/grounding/search", async (c) => {
    const q = (c.req.query("q") ?? "").trim();
    if (!q) return c.json({ error: "q is required" }, 400);
    const m = engine.matcher.match(q);
    return c.json({
      trigram_hits: await store.q().searchSnippets(q, 10),
      matcher_direct: m.direct?.slug,
      matcher_context: m.context.map((s) => s.slug),
    });
  });
  admin.post("/grounding/reload", async (c) => {
    await engine.loadGrounding();
    return c.json({ snippets: engine.matcher.size });
  });

  // Development simulator (mock gateway only).
  if (isDevelopment(cfg) && a.mockGateway) {
    admin.post("/simulate/channel", async (c) => {
      const parsed = z.object({ channel: z.literal("sms"), from: z.string(), body: z.string() }).safeParse(await c.req.json().catch(() => null));
      if (!parsed.success) return c.json({ error: 'need {"channel": "sms", "from", "body"}' }, 400);
      try {
        const res = await engine.handleInboundSMS({
          provider: "mock",
          providerEventId: "sim-" + randomBytes(8).toString("hex"),
          serviceId: cfg.smsServiceId,
          from: parsed.data.from,
          body: parsed.data.body,
          receivedAt: new Date(),
          environment: "simulator",
        });
        return c.json(res);
      } catch (err) {
        return c.json({ error: (err as Error).message }, 400);
      }
    });
    admin.get("/simulate/outbox", async (c) => {
      const phone = tryNormalize(c.req.query("phone") ?? "");
      if (!phone) return c.json({ error: "phone is required" }, 400);
      const user = await store.q().findUserByPhone(phone);
      if (!user) return c.json({ messages: [] });
      return c.json({ messages: await store.q().outbox(user.id, 40), bonus_quota: user.bonusQuota, opted_out: user.optedOut });
    });
    const pagePath = resolveAsset("public/simulator.html");
    const page = pagePath ? readFileSync(pagePath, "utf8") : "<p>simulator.html not found</p>";
    app.get("/simulator", (c) =>
      c.html(page, 200, {
        "Content-Security-Policy": "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'",
      }),
    );
    app.get("/", (c) => c.redirect("/simulator"));
  }

  app.route("/admin", admin);
  app.notFound((c) => c.json({ error: "not found" }, 404));

  return { app, close: async () => void (await sub?.unlisten().catch(() => {})) };
}
