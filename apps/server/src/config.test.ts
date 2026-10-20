import { describe, expect, it } from "vitest";
import { allowsMock, loadConfig, parsePrices, validate } from "./config";

const keys = {
  ENCRYPTION_KEYS_V1: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
  HMAC_KEYS_V1: "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
};
const tokens = { ADMIN_API_TOKEN: "0123456789abcdef01234567", METRICS_TOKEN: "0123456789abcdef01234567" };

describe("config", () => {
  it("has no simulator or gateway by default", () => {
    const cfg = loadConfig({ APP_ENV: "development" });
    expect(cfg.llmProvider).toBe("deepinfra");
    expect(cfg.primarySmsProvider).toBe("");
    expect(cfg.insecureDevKeys).toBe(true);
    const errs = validate(cfg).join("\n");
    expect(errs).toContain("DEEPINFRA_API_KEY");
    expect(errs).toContain("PRIMARY_SMS_PROVIDER is required");
  });

  it("allows mock only in development and test", () => {
    const mock = { LLM_PROVIDER: "mock", PRIMARY_SMS_PROVIDER: "mock" };
    for (const env of ["development", "test"]) {
      const cfg = loadConfig({ APP_ENV: env, ...keys, ...mock });
      expect(allowsMock(cfg)).toBe(true);
      expect(validate(cfg)).toEqual([]);
    }
    for (const env of ["staging", "production"]) {
      const cfg = loadConfig({ APP_ENV: env, ...keys, ...tokens, ...mock });
      expect(allowsMock(cfg)).toBe(false);
      expect(validate(cfg).filter((e) => e.includes("only allowed when APP_ENV is development or test"))).toHaveLength(2);
    }
  });

  it("requires a strong device secret for the gateway", () => {
    const base = { APP_ENV: "production", ...keys, ...tokens, DEEPINFRA_API_KEY: "k", PRIMARY_SMS_PROVIDER: "gateway" };
    expect(validate(loadConfig(base)).join()).toContain("GATEWAY_DEVICE_SECRET");
    expect(validate(loadConfig({ ...base, GATEWAY_DEVICE_SECRET: "short" })).join()).toContain("at least 24");
    expect(validate(loadConfig({ ...base, GATEWAY_DEVICE_SECRET: "REPLACE_ME" })).join()).toContain("GATEWAY_DEVICE_SECRET");
    const ok = loadConfig({ ...base, GATEWAY_DEVICE_SECRET: "a".repeat(24) + ", " + "b".repeat(30) });
    expect(ok.gatewayDeviceSecrets).toHaveLength(2);
    expect(validate(ok)).toEqual([]);
  });

  it("rejects malformed values", () => {
    expect(() => loadConfig({ WORKER_CONCURRENCY: "four" })).toThrow(/not an integer/);
    expect(() => loadConfig({ AUTO_MIGRATE: "maybe" })).toThrow(/not a boolean/);
    expect(() => loadConfig({ ENCRYPTION_KEYS_V1: "***" })).toThrow(/base64/);
  });

  it("parses LLM prices", () => {
    expect(parsePrices("a/b=0.1:0.2, c=1:2").get("c")).toEqual({ inputPerM: 1, outputPerM: 2 });
    expect(() => parsePrices("bad")).toThrow();
  });
});
