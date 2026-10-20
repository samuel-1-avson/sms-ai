// Runtime configuration from environment variables. Every setting has a
// development-friendly default; validate() enforces the stricter rules that
// apply outside development.
import { createHash } from "node:crypto";

export interface ModelPrice {
  inputPerM: number;
  outputPerM: number;
}

export type AppEnv = "development" | "test" | "staging" | "production";

export interface Config {
  env: AppEnv;
  httpAddr: string;
  databaseUrl: string;
  autoMigrate: boolean;
  embeddedWorker: boolean;
  logLevel: string;

  currentKeyVersion: number;
  encryptionKeys: Map<number, Buffer>;
  hmacKeys: Map<number, Buffer>;
  insecureDevKeys: boolean;

  /** "gateway" (the Android phone) or "mock" (development and test only). */
  primarySmsProvider: string;
  smsServiceId: string;
  inboundDebounceMs: number;
  coalesceExtendMs: number;
  maxCoalesce: number;
  mockWebhookSecret: string;
  mockDlrCallbackUrl: string;

  /** Device secrets for /gateway/v1/*; two while rotating, the first is current. */
  gatewayDeviceSecrets: string[];
  /** The gateway SIM's own number (E.164 or local), so texts from it are never answered. */
  gatewayPhoneNumber: string;
  /** Public origin the phone uses, printed in the pairing QR code. */
  gatewayPublicUrl: string;
  gatewayOutboxTtlSeconds: number;
  gatewayAckTimeoutSeconds: number;
  gatewayOfflineAfterSeconds: number;

  llmProvider: string;
  deepInfraBaseUrl: string;
  deepInfraApiKey: string;
  llmModel: string;
  llmFallbackModel: string;
  llmReasoningEffort: string;
  llmMaxOutputTokens: number;
  llmTemperature: number;
  llmTimeoutMs: number;
  llmPrices: Map<string, ModelPrice>;
  mockLlmLatencyMs: number;

  dailyPerNumberFreeCap: number;
  globalDailySmsSegmentCap: number;
  globalDailySpendCapUsd: number;
  smsSegmentCostUsd: number;
  contextTtlSeconds: number;
  contextMaxTurns: number;
  contextMaxInputTokens: number;
  retentionDays: number;
  maxMorePages: number;
  perNumberRatePerMinute: number;
  maxFailedCodeAttempts: number;
  allowUcs2LocalLanguage: boolean;

  adminApiToken: string;
  metricsToken: string;

  groundingSeedPath: string;
  groundingRefreshSeconds: number;

  workerConcurrency: number;
  workerPollMs: number;
  jobLeaseSeconds: number;
  jobMaxAttempts: number;
}

export const isDevelopment = (c: Config) => c.env === "development";
/** Whether simulators (mock gateway, mock model) are allowed. */
export const allowsMock = (c: Config) => c.env === "development" || c.env === "test";

class Reader {
  errs: string[] = [];
  constructor(private readonly env: Record<string, string | undefined>) {}

  str(key: string, def: string): string {
    const v = this.env[key]?.trim();
    return v ? v : def;
  }
  /** "" for unset values and for placeholders copied from .env.example. */
  secret(key: string): string {
    const v = this.str(key, "");
    return isPlaceholder(v) ? "" : v;
  }
  secretList(key: string): string[] {
    return this.str(key, "")
      .split(",")
      .map((v) => v.trim())
      .filter((v) => v !== "" && !isPlaceholder(v));
  }
  int(key: string, def: number): number {
    const v = this.str(key, "");
    if (v === "") return def;
    if (!/^-?\d+$/.test(v)) {
      this.errs.push(`${key}: "${v}" is not an integer`);
      return def;
    }
    return Number(v);
  }
  float(key: string, def: number): number {
    const v = this.str(key, "");
    if (v === "") return def;
    const f = Number(v);
    if (!Number.isFinite(f)) {
      this.errs.push(`${key}: "${v}" is not a number`);
      return def;
    }
    return f;
  }
  bool(key: string, def: boolean): boolean {
    const v = this.str(key, "").toLowerCase();
    if (v === "") return def;
    if (["1", "true", "yes", "on"].includes(v)) return true;
    if (["0", "false", "no", "off"].includes(v)) return false;
    this.errs.push(`${key}: "${v}" is not a boolean`);
    return def;
  }
}

export function isPlaceholder(v: string): boolean {
  const u = v.trim().toUpperCase();
  return u.startsWith("REPLACE_") || u.startsWith("CHANGE_ME") || u.startsWith("SET_AFTER");
}

function decodeKey(v: string): Buffer {
  const s = v.trim();
  if (!/^[A-Za-z0-9+/_-]+={0,2}$/.test(s)) throw new Error("value is not valid base64");
  return Buffer.from(s.replace(/-/g, "+").replace(/_/g, "/"), "base64");
}

function devKey(purpose: string): Buffer {
  return createHash("sha256").update("smsai-ghana-INSECURE-development-only-" + purpose).digest();
}

/** Parses "model=in:out,model2=in:out" (USD per 1M tokens). */
export function parsePrices(s: string): Map<string, ModelPrice> {
  const out = new Map<string, ModelPrice>();
  for (let item of s.split(",")) {
    item = item.trim();
    if (!item) continue;
    const eq = item.indexOf("=");
    const pair = eq >= 0 ? item.slice(eq + 1) : "";
    const colon = pair.indexOf(":");
    if (eq < 0 || colon < 0) throw new Error(`LLM_PRICES: "${item}" must look like model=in:out`);
    const inF = Number(pair.slice(0, colon).trim());
    const outF = Number(pair.slice(colon + 1).trim());
    if (!Number.isFinite(inF) || !Number.isFinite(outF) || inF < 0 || outF < 0) {
      throw new Error(`LLM_PRICES: "${item}" has invalid prices`);
    }
    out.set(item.slice(0, eq).trim(), { inputPerM: inF, outputPerM: outF });
  }
  return out;
}

/** Reads configuration from an environment map (process.env by default). */
export function loadConfig(env: Record<string, string | undefined> = process.env): Config {
  const r = new Reader(env);
  const appEnv = r.str("APP_ENV", "development") as AppEnv;
  const c: Config = {
    env: appEnv,
    httpAddr: r.str("HTTP_ADDR", ":8080"),
    databaseUrl: r.str("DATABASE_URL", "postgres://smsai:smsai_dev_password@localhost:5442/smsai?sslmode=disable"),
    autoMigrate: r.bool("AUTO_MIGRATE", appEnv === "development"),
    embeddedWorker: r.bool("EMBEDDED_WORKER", false),
    logLevel: r.str("LOG_LEVEL", "info"),

    currentKeyVersion: r.int("CURRENT_KEY_VERSION", 1),
    encryptionKeys: new Map(),
    hmacKeys: new Map(),
    insecureDevKeys: false,

    // No default: the gateway must be chosen explicitly, and forgetting to
    // fails validation instead of falling back to a simulator.
    primarySmsProvider: r.str("PRIMARY_SMS_PROVIDER", ""),
    smsServiceId: r.str("SMS_SERVICE_ID", "GATEWAY-SIM"),
    inboundDebounceMs: r.int("INBOUND_DEBOUNCE_MS", 2500),
    coalesceExtendMs: r.int("INBOUND_COALESCE_EXTEND_MS", 1500),
    maxCoalesce: r.int("INBOUND_MAX_COALESCE", 3),
    mockWebhookSecret: r.str("MOCK_WEBHOOK_SECRET", "dev-mock-webhook-secret"),
    mockDlrCallbackUrl: r.str("MOCK_DLR_CALLBACK_URL", ""),

    gatewayDeviceSecrets: r.secretList("GATEWAY_DEVICE_SECRET"),
    gatewayPhoneNumber: r.str("GATEWAY_PHONE_NUMBER", ""),
    gatewayPublicUrl: r.str("GATEWAY_PUBLIC_URL", "").replace(/\/+$/, ""),
    gatewayOutboxTtlSeconds: r.int("GATEWAY_OUTBOX_TTL_SECONDS", 6 * 3600),
    gatewayAckTimeoutSeconds: r.int("GATEWAY_ACK_TIMEOUT_SECONDS", 1800),
    gatewayOfflineAfterSeconds: r.int("GATEWAY_OFFLINE_AFTER_SECONDS", 300),

    llmProvider: r.str("LLM_PROVIDER", "deepinfra"),
    deepInfraBaseUrl: r.str("DEEPINFRA_BASE_URL", "https://api.deepinfra.com/v1/openai").replace(/\/+$/, ""),
    deepInfraApiKey: r.secret("DEEPINFRA_API_KEY"),
    // Chosen after the 2026-10-17 benchmark: gpt-oss-20b, Llama 3.3 70B as fallback.
    llmModel: r.str("LLM_MODEL", "openai/gpt-oss-20b"),
    llmFallbackModel: r.str("LLM_FALLBACK_MODEL", "meta-llama/Llama-3.3-70B-Instruct-Turbo"),
    llmReasoningEffort: r.str("LLM_REASONING_EFFORT", "low"),
    llmMaxOutputTokens: r.int("LLM_MAX_OUTPUT_TOKENS", 300),
    llmTemperature: r.float("LLM_TEMPERATURE", 0.2),
    llmTimeoutMs: r.int("LLM_TIMEOUT_MS", 15000),
    llmPrices: new Map(),
    mockLlmLatencyMs: r.int("MOCK_LLM_LATENCY_MS", 400),

    dailyPerNumberFreeCap: r.int("DAILY_PER_NUMBER_FREE_CAP", 15),
    // Low by default: every segment goes out from a personal SIM.
    globalDailySmsSegmentCap: r.int("GLOBAL_DAILY_SMS_SEGMENT_CAP", 200),
    globalDailySpendCapUsd: r.float("GLOBAL_DAILY_SPEND_CAP_USD", 15.0),
    smsSegmentCostUsd: r.float("SMS_SEGMENT_COST_USD", 0.025),
    contextTtlSeconds: r.int("CONTEXT_TTL_HOURS", 24) * 3600,
    contextMaxTurns: r.int("CONTEXT_MAX_TURNS", 6),
    contextMaxInputTokens: r.int("CONTEXT_MAX_INPUT_TOKENS", 800),
    retentionDays: r.int("RETENTION_DAYS", 30),
    maxMorePages: r.int("MAX_MORE_PAGES", 1),
    perNumberRatePerMinute: r.int("PER_NUMBER_RATE_LIMIT_PER_MINUTE", 6),
    maxFailedCodeAttempts: r.int("MAX_FAILED_CODE_ATTEMPTS_PER_DAY", 5),
    allowUcs2LocalLanguage: r.bool("ALLOW_UCS2_LOCAL_LANGUAGE", true),

    adminApiToken: r.secret("ADMIN_API_TOKEN"),
    metricsToken: r.secret("METRICS_TOKEN"),

    groundingSeedPath: r.str("GROUNDING_SEED_PATH", ""),
    groundingRefreshSeconds: r.int("GROUNDING_REFRESH_SECONDS", 300),

    workerConcurrency: r.int("WORKER_CONCURRENCY", 4),
    workerPollMs: r.int("WORKER_POLL_MS", 500),
    jobLeaseSeconds: r.int("JOB_LEASE_SECONDS", 60),
    jobMaxAttempts: r.int("JOB_MAX_ATTEMPTS", 3),
  };

  try {
    c.llmPrices = parsePrices(r.str("LLM_PRICES", ""));
  } catch (err) {
    r.errs.push((err as Error).message);
  }

  for (const [k, v] of Object.entries(env)) {
    if (v === undefined || isPlaceholder(v)) continue;
    let target: Map<number, Buffer>;
    let suffix: string;
    if (k.startsWith("ENCRYPTION_KEYS_V")) [target, suffix] = [c.encryptionKeys, k.slice("ENCRYPTION_KEYS_V".length)];
    else if (k.startsWith("HMAC_KEYS_V")) [target, suffix] = [c.hmacKeys, k.slice("HMAC_KEYS_V".length)];
    else continue;
    const version = Number(suffix);
    if (!/^\d+$/.test(suffix) || version < 1 || version > 32767) {
      r.errs.push(`${k}: key version suffix must be a positive integer`);
      continue;
    }
    try {
      target.set(version, decodeKey(v));
    } catch (err) {
      r.errs.push(`${k}: ${(err as Error).message}`);
    }
  }
  if (c.encryptionKeys.size === 0 && c.hmacKeys.size === 0 && c.env === "development") {
    // Deterministic, publicly known keys so a local database survives
    // restarts. validate() refuses them outside development.
    c.encryptionKeys.set(c.currentKeyVersion, devKey("encryption"));
    c.hmacKeys.set(c.currentKeyVersion, devKey("hmac"));
    c.insecureDevKeys = true;
  }

  if (r.errs.length > 0) throw new Error(r.errs.join("\n"));
  return c;
}

/** Cross-field rules. Production rules are deliberately strict. Returns the problems found. */
export function validate(c: Config): string[] {
  const errs: string[] = [];
  const add = (m: string) => errs.push(m);

  if (!["development", "staging", "production", "test"].includes(c.env)) {
    add(`APP_ENV must be development, test, staging or production (got "${c.env}")`);
  }
  if (!c.databaseUrl) add("DATABASE_URL is required");
  const enc = c.encryptionKeys.get(c.currentKeyVersion);
  if (!enc) add(`ENCRYPTION_KEYS_V${c.currentKeyVersion} is required for CURRENT_KEY_VERSION=${c.currentKeyVersion}`);
  for (const [v, k] of c.encryptionKeys) if (k.length !== 32) add(`ENCRYPTION_KEYS_V${v} must decode to exactly 32 bytes`);
  const mac = c.hmacKeys.get(c.currentKeyVersion);
  if (!mac) add(`HMAC_KEYS_V${c.currentKeyVersion} is required for CURRENT_KEY_VERSION=${c.currentKeyVersion}`);
  else if (mac.length < 32) add(`HMAC_KEYS_V${c.currentKeyVersion} must decode to at least 32 bytes`);

  switch (c.llmProvider) {
    case "deepinfra":
      if (!c.deepInfraApiKey) add("DEEPINFRA_API_KEY is required when LLM_PROVIDER=deepinfra");
      if (!c.llmModel) add("LLM_MODEL is required");
      break;
    case "mock":
      break;
    default:
      add(`LLM_PROVIDER must be deepinfra or mock (got "${c.llmProvider}")`);
  }
  if (c.inboundDebounceMs < 0 || c.coalesceExtendMs < 0) add("debounce durations must not be negative");
  if (c.maxMorePages < 0) add("MAX_MORE_PAGES must not be negative");
  if (c.workerConcurrency < 1) add("WORKER_CONCURRENCY must be at least 1");
  if (c.jobMaxAttempts < 1) add("JOB_MAX_ATTEMPTS must be at least 1");
  if (c.globalDailySpendCapUsd <= 0 || c.globalDailySmsSegmentCap <= 0 || c.dailyPerNumberFreeCap < 0) {
    add("spend and segment caps must be positive");
  }

  switch (c.primarySmsProvider) {
    case "":
      add('PRIMARY_SMS_PROVIDER is required: "gateway" (the Android phone), or "mock" in development and test');
      break;
    case "gateway":
      if (c.gatewayDeviceSecrets.length === 0) {
        add("GATEWAY_DEVICE_SECRET (24+ random characters) is required when PRIMARY_SMS_PROVIDER=gateway");
      }
      for (const s of c.gatewayDeviceSecrets) {
        if (s.length < 24) add("every GATEWAY_DEVICE_SECRET value must be at least 24 characters");
      }
      if (c.gatewayPublicUrl && !/^https?:\/\/[^/]+$/.test(c.gatewayPublicUrl)) {
        add("GATEWAY_PUBLIC_URL must be an origin like https://sms.example.com (no path)");
      }
      if (c.gatewayOutboxTtlSeconds < 60) add("GATEWAY_OUTBOX_TTL_SECONDS must be at least 60");
      break;
    case "mock":
      break;
    default:
      add(`PRIMARY_SMS_PROVIDER must be gateway or mock (got "${c.primarySmsProvider}")`);
  }

  if (c.env !== "development" && c.env !== "test") {
    if (c.insecureDevKeys) add(`insecure development keys are not allowed when APP_ENV=${c.env}`);
    if (c.adminApiToken.length < 24) add(`ADMIN_API_TOKEN (24+ chars) is required when APP_ENV=${c.env}`);
    if (c.metricsToken.length < 24) add(`METRICS_TOKEN (24+ chars) is required when APP_ENV=${c.env}`);
  }
  if (!allowsMock(c)) {
    if (c.llmProvider === "mock") add(`LLM_PROVIDER=mock is only allowed when APP_ENV is development or test (APP_ENV=${c.env})`);
    if (c.primarySmsProvider === "mock") add(`PRIMARY_SMS_PROVIDER=mock is only allowed when APP_ENV is development or test (APP_ENV=${c.env})`);
  }
  return errs;
}

/** Configured key versions in ascending order. */
export function keyVersions(c: Config): number[] {
  return [...new Set([...c.encryptionKeys.keys(), ...c.hmacKeys.keys()])].sort((a, b) => a - b);
}

/** The port part of HTTP_ADDR (":8080" or "0.0.0.0:8080"). */
export function httpPort(c: Config): number {
  const port = Number(c.httpAddr.slice(c.httpAddr.lastIndexOf(":") + 1));
  return Number.isInteger(port) && port > 0 ? port : 8080;
}
