// Builds a complete application against a throwaway PostgreSQL database for
// integration tests. They are skipped unless SMSAI_TEST_DATABASE_URL points
// at a server where the user may create databases, e.g.
// postgres://smsai:smsai_dev_password@localhost:5442/postgres
import { pino } from "pino";
import postgres from "postgres";
import { buildApp, type App } from "../app";
import { loadConfig, type Config } from "../config";
import type { MockGateway } from "../channel/mock";
import type { MockModel } from "../ai";
import type { InboundResult } from "../service/engine";

export const TEST_DB = process.env.SMSAI_TEST_DATABASE_URL ?? "";
export const TEST_ENCRYPTION_KEY = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=";
export const TEST_HMAC_KEY = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=";
export const SHORT_CODE = "GATEWAY-SIM";

/** Short timers so debounce and retries can be exercised quickly. */
export function baseEnviron(dbUrl: string): Record<string, string> {
  return {
    APP_ENV: "test",
    DATABASE_URL: dbUrl,
    AUTO_MIGRATE: "true",
    LOG_LEVEL: "error",
    CURRENT_KEY_VERSION: "1",
    ENCRYPTION_KEYS_V1: TEST_ENCRYPTION_KEY,
    HMAC_KEYS_V1: TEST_HMAC_KEY,
    INBOUND_DEBOUNCE_MS: "300",
    INBOUND_COALESCE_EXTEND_MS: "300",
    MOCK_LLM_LATENCY_MS: "20",
    LLM_TIMEOUT_MS: "4000",
    WORKER_POLL_MS: "50",
    PER_NUMBER_RATE_LIMIT_PER_MINUTE: "100",
    GLOBAL_DAILY_SMS_SEGMENT_CAP: "500",
    SMS_SERVICE_ID: SHORT_CODE,
    LLM_PROVIDER: "mock",
    PRIMARY_SMS_PROVIDER: "mock",
  };
}

/** Recreates database `name` on the test server and returns its URL. */
export async function freshDatabase(name: string): Promise<string> {
  name += process.env.SMSAI_TEST_DB_SUFFIX ?? "";
  const admin = postgres(TEST_DB, { max: 1, onnotice: () => {} });
  try {
    await admin.unsafe(`DROP DATABASE IF EXISTS "${name}" WITH (FORCE)`);
    await admin.unsafe(`CREATE DATABASE "${name}"`);
  } finally {
    await admin.end();
  }
  const u = new URL(TEST_DB);
  u.pathname = "/" + name;
  return u.toString();
}

/** Returns environ with the key settings replaced. */
export function keyEnviron(environ: Record<string, string>, current: number, keys: Record<number, [string, string]>) {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(environ)) {
    if (!k.startsWith("ENCRYPTION_KEYS_V") && !k.startsWith("HMAC_KEYS_V") && k !== "CURRENT_KEY_VERSION") out[k] = v;
  }
  out.CURRENT_KEY_VERSION = String(current);
  for (const [v, [enc, mac]] of Object.entries(keys)) {
    out[`ENCRYPTION_KEYS_V${v}`] = enc;
    out[`HMAC_KEYS_V${v}`] = mac;
  }
  return out;
}

/** A base64 32-byte key derived from a seed byte. */
export function derivedKey(seed: number): string {
  return Buffer.from(Array.from({ length: 32 }, (_, i) => (seed + i) & 0xff)).toString("base64");
}

export class TestEnv {
  private seq = 0;

  private constructor(
    readonly app: App,
    readonly dbUrl: string,
  ) {}

  static async create(dbName: string, mutate?: (c: Config) => void, environ?: (dbUrl: string) => Record<string, string>): Promise<TestEnv> {
    const dbUrl = await freshDatabase(dbName);
    return TestEnv.on(dbUrl, (environ ?? baseEnviron)(dbUrl), mutate);
  }

  /** Builds an app on an existing database with an explicit environment. */
  static async on(dbUrl: string, env: Record<string, string>, mutate?: (c: Config) => void): Promise<TestEnv> {
    const cfg = loadConfig(env);
    mutate?.(cfg);
    const app = await buildApp(cfg, { log: pino({ level: "silent" }) });
    if (app.mockModel) app.mockModel.slowLatencyMs = 1500;
    app.engine.reconcileAfterSeconds = 0;
    return new TestEnv(app, dbUrl);
  }

  get engine() {
    return this.app.engine;
  }
  get store() {
    return this.app.store;
  }
  get model(): MockModel {
    return this.app.mockModel!;
  }
  get gw(): MockGateway {
    return this.app.mockGateway!;
  }

  /** Delivers an inbound text as if from the mock provider. */
  sms(from: string, body: string, eventId = `evt-${Date.now()}-${++this.seq}`): Promise<InboundResult> {
    return this.engine.handleInboundSMS({
      provider: "mock",
      providerEventId: eventId,
      serviceId: SHORT_CODE,
      from,
      body,
      receivedAt: new Date(),
      environment: "test",
    });
  }

  /** Processes jobs until none are pending or retrying; debounced jobs are waited for. */
  async drain(timeoutMs = 5000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const job = await this.store.q().claimJob("test-worker", 60);
      if (job) {
        try {
          await this.engine.processJob(job);
        } catch (err) {
          await this.engine.rescheduleAfterError(job, err);
        }
        continue;
      }
      const open = await this.count(`SELECT count(*)::int FROM jobs WHERE status = 'pending' OR (status = 'running' AND attempts < max_attempts)`);
      if (open === 0) return;
      await new Promise((r) => setTimeout(r, 40));
    }
    throw new Error(`jobs still open after ${timeoutMs} ms`);
  }

  sentBodies(): string[] {
    return this.gw.sent.map((m) => m.body);
  }

  lastSent(): string {
    const b = this.sentBodies();
    if (b.length === 0) throw new Error("no SMS was sent");
    return b[b.length - 1]!;
  }

  /** Runs a single-value query (count, id, ...). */
  async count(sql: string, ...params: unknown[]): Promise<number> {
    const rows = await this.store.sql.unsafe(sql, params as never[]);
    return Number(Object.values(rows[0] ?? { n: 0 })[0]);
  }

  async value(sql: string, ...params: unknown[]): Promise<string> {
    const rows = await this.store.sql.unsafe(sql, params as never[]);
    return String(Object.values(rows[0] ?? {})[0] ?? "");
  }

  close(): Promise<void> {
    return this.app.close();
  }
}
