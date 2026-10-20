// Wiring shared by the api, worker and admin entry points.
import type { Logger } from "pino";
import { Client, DeepInfra, MockModel, type ChatModel } from "./ai";
import { Registry } from "./channel/channel";
import { GatewayChannel } from "./channel/gateway";
import { MockGateway } from "./channel/mock";
import { allowsMock, isDevelopment, type Config } from "./config";
import { defaultSeedPath, loadSeedFile, Matcher, type Snippet } from "./grounding";
import { Metrics, newLogger } from "./observability";
import { Store } from "./repository/store";
import { Keyring } from "./security/keyring";
import { Engine } from "./service/engine";

export interface App {
  cfg: Config;
  log: Logger;
  store: Store;
  metrics: Metrics;
  registry: Registry;
  ai: Client;
  engine: Engine;
  /** Set only when PRIMARY_SMS_PROVIDER=mock (development and test). */
  mockGateway: MockGateway | null;
  /** Set only when LLM_PROVIDER=mock. */
  mockModel: MockModel | null;
  gateway: GatewayChannel | null;
  close(): Promise<void>;
}

export function newKeyring(cfg: Config): Keyring {
  return new Keyring(cfg.currentKeyVersion, cfg.encryptionKeys, cfg.hmacKeys);
}

export interface BuildOptions {
  log?: Logger;
  /** Skip loading grounding (admin commands that do not need it). */
  skipGrounding?: boolean;
}

export async function buildApp(cfg: Config, opts: BuildOptions = {}): Promise<App> {
  const log = opts.log ?? newLogger(cfg.logLevel);
  const store = await openWithRetry(cfg.databaseUrl, newKeyring(cfg), log);
  try {
    if (cfg.autoMigrate) {
      const applied = await store.migrate();
      if (applied.length > 0) log.info({ versions: applied }, "migrations applied");
    }
    const metrics = new Metrics();
    metrics.dailySpendCapUsd.set(cfg.globalDailySpendCapUsd);

    const registry = new Registry(cfg.primarySmsProvider);
    let mockGateway: MockGateway | null = null;
    let gateway: GatewayChannel | null = null;
    if (cfg.primarySmsProvider === "mock") {
      if (!allowsMock(cfg)) throw new Error(`PRIMARY_SMS_PROVIDER=mock is not allowed when APP_ENV=${cfg.env}`);
      mockGateway = new MockGateway(cfg.mockWebhookSecret, cfg.mockDlrCallbackUrl, log);
      registry.addInbound(mockGateway);
      registry.addSender(mockGateway);
    } else if (cfg.primarySmsProvider === "gateway") {
      gateway = new GatewayChannel(store, { secrets: cfg.gatewayDeviceSecrets, serviceId: cfg.smsServiceId, ownNumber: cfg.gatewayPhoneNumber });
      registry.addInbound(gateway);
      registry.addSender(gateway);
      if (!cfg.gatewayPhoneNumber) log.warn("GATEWAY_PHONE_NUMBER is not set: texts from the gateway SIM itself are not filtered");
    } else {
      throw new Error(`PRIMARY_SMS_PROVIDER="${cfg.primarySmsProvider}" has no adapter in this build`);
    }

    let model: ChatModel;
    let mockModel: MockModel | null = null;
    if (cfg.llmProvider === "deepinfra") {
      model = new DeepInfra(cfg.deepInfraBaseUrl, cfg.deepInfraApiKey);
    } else if (cfg.llmProvider === "mock") {
      if (!allowsMock(cfg)) throw new Error(`LLM_PROVIDER=mock is not allowed when APP_ENV=${cfg.env}`);
      model = mockModel = new MockModel(cfg.mockLlmLatencyMs);
    } else {
      throw new Error(`LLM_PROVIDER="${cfg.llmProvider}" has no adapter in this build`);
    }
    const ai = new Client(model, {
      primary: cfg.llmModel,
      fallback: cfg.llmFallbackModel,
      reasoningEffort: cfg.llmReasoningEffort,
      maxOutputTokens: cfg.llmMaxOutputTokens,
      temperature: cfg.llmTemperature,
      prices: cfg.llmPrices,
      observer: metrics,
    });

    const engine = new Engine({ cfg, store, registry, ai, matcher: new Matcher(), metrics, log });
    if (!opts.skipGrounding) await engine.loadGrounding(seed(cfg));

    return { cfg, log, store, metrics, registry, ai, engine, mockGateway, mockModel, gateway, close: () => store.close() };
  } catch (err) {
    await store.close();
    throw err;
  }
}

/** Snippets to upsert at start-up: always in development and test, elsewhere only when GROUNDING_SEED_PATH is set. */
function seed(cfg: Config): Snippet[] {
  if (cfg.groundingSeedPath) return loadSeedFile(cfg.groundingSeedPath);
  if (isDevelopment(cfg) || cfg.env === "test") {
    const p = defaultSeedPath();
    return p ? loadSeedFile(p) : [];
  }
  return [];
}

async function openWithRetry(url: string, kr: Keyring, log: Logger): Promise<Store> {
  let lastErr: unknown;
  for (let attempt = 1; attempt <= 15; attempt++) {
    const store = new Store(url, kr);
    try {
      await store.ping();
      return store;
    } catch (err) {
      lastErr = err;
      await store.close().catch(() => {});
      log.warn({ attempt, err: (err as Error).message }, "database not ready; retrying");
      await new Promise((r) => setTimeout(r, 2000));
    }
  }
  throw new Error(`connecting to database: ${(lastErr as Error).message}`);
}
