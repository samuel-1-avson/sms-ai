// Timestamped HMAC-SHA256 request signatures, shared by the server and the
// Android gateway app. The signature covers "<unix seconds>.<raw body>" and
// is sent as "sha256=<hex>" (the same format as the Go mock gateway).
// @noble/hashes is pure JS, so this runs unchanged in Node and React Native.
import { hmac } from "@noble/hashes/hmac";
import { sha256 } from "@noble/hashes/sha2";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils";

export const HEADER_SIGNATURE = "X-Webhook-Signature";
export const HEADER_TIMESTAMP = "X-Webhook-Timestamp";
export const DEFAULT_TOLERANCE_SECONDS = 300;

export type SignatureError = "missing" | "stale" | "mismatch";

function toBytes(body: string | Uint8Array): Uint8Array {
  return typeof body === "string" ? utf8ToBytes(body) : body;
}

/** Returns "sha256=<hex>" over "<timestamp>.<body>". */
export function sign(secret: string, timestamp: number, body: string | Uint8Array): string {
  const prefix = utf8ToBytes(`${Math.trunc(timestamp)}.`);
  const payload = toBytes(body);
  const msg = new Uint8Array(prefix.length + payload.length);
  msg.set(prefix, 0);
  msg.set(payload, prefix.length);
  return "sha256=" + bytesToHex(hmac(sha256, utf8ToBytes(secret), msg));
}

/** Constant-time string comparison (length is not secret). */
export function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

/**
 * Checks a signature over the raw body. It must run before the body is
 * parsed (plan §10.1). Returns null when valid, otherwise the reason.
 */
export function verify(
  secret: string,
  signatureHeader: string | null | undefined,
  timestampHeader: string | null | undefined,
  body: string | Uint8Array,
  nowSeconds: number,
  toleranceSeconds = DEFAULT_TOLERANCE_SECONDS,
): SignatureError | null {
  if (!signatureHeader || !timestampHeader) return "missing";
  const ts = Number(timestampHeader.trim());
  if (!Number.isInteger(ts)) return "mismatch";
  if (Math.abs(nowSeconds - ts) > toleranceSeconds) return "stale";
  return constantTimeEqual(sign(secret, ts, body), signatureHeader.trim()) ? null : "mismatch";
}

/** Headers for a signed request sent now. */
export function signedHeaders(secret: string, body: string, nowSeconds = Math.floor(Date.now() / 1000)): Record<string, string> {
  return {
    "Content-Type": "application/json",
    [HEADER_TIMESTAMP]: String(nowSeconds),
    [HEADER_SIGNATURE]: sign(secret, nowSeconds, body),
  };
}
