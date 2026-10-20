import { createHmac } from "node:crypto";
import { describe, expect, it } from "vitest";
import { constantTimeEqual, sign, signedHeaders, verify, HEADER_SIGNATURE, HEADER_TIMESTAMP } from "./signing";

const secret = "test-secret-at-least-24-characters";
const body = '{"messages":[{"id":"abc","text":"Ɛte sɛn?"}]}';

describe("sign", () => {
  it("matches HMAC-SHA256 over '<ts>.<body>' (the Go format)", () => {
    const expected = "sha256=" + createHmac("sha256", secret).update(`1700000000.${body}`).digest("hex");
    expect(sign(secret, 1700000000, body)).toBe(expected);
    expect(sign(secret, 1700000000, new TextEncoder().encode(body))).toBe(expected);
  });
});

describe("cross-language vector", () => {
  it("matches the Kotlin gateway (GatewayClientTest.kt)", () => {
    expect(sign(secret, 1700000000, '{"messages":[{"body":"Ɛte sɛn?"}]}')).toBe(
      "sha256=59eb535ca2774660bb686e8c1016d4db4d11c3a40c84a922e32a7ea21294c5f3",
    );
  });
});

describe("verify", () => {
  const now = 1700000000;
  const sig = sign(secret, now, body);

  it("accepts a fresh, correct signature", () => {
    expect(verify(secret, sig, String(now), body, now + 10)).toBeNull();
  });
  it("rejects missing headers", () => {
    expect(verify(secret, "", String(now), body, now)).toBe("missing");
    expect(verify(secret, sig, undefined, body, now)).toBe("missing");
  });
  it("rejects stale timestamps", () => {
    expect(verify(secret, sig, String(now), body, now + 301)).toBe("stale");
    expect(verify(secret, sig, String(now), body, now - 301)).toBe("stale");
  });
  it("rejects tampered bodies, wrong secrets and garbage timestamps", () => {
    expect(verify(secret, sig, String(now), body + " ", now)).toBe("mismatch");
    expect(verify("other-secret-at-least-24-chars", sig, String(now), body, now)).toBe("mismatch");
    expect(verify(secret, sig, "12x", body, now)).toBe("mismatch");
  });
});

describe("helpers", () => {
  it("constantTimeEqual", () => {
    expect(constantTimeEqual("abc", "abc")).toBe(true);
    expect(constantTimeEqual("abc", "abd")).toBe(false);
    expect(constantTimeEqual("abc", "ab")).toBe(false);
  });
  it("signedHeaders verify", () => {
    const h = signedHeaders(secret, body, 1700000000);
    expect(verify(secret, h[HEADER_SIGNATURE], h[HEADER_TIMESTAMP], body, 1700000000)).toBeNull();
  });
});
