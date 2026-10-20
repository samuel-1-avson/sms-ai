// A signed, provider-shaped SMS gateway for local development, fixtures and
// tests. It exercises the same raw-body HMAC verification a real gateway
// uses (headers X-Webhook-Timestamp / X-Webhook-Signature).
import { randomBytes } from "node:crypto";
import { signedHeaders, verify } from "@smsai/shared";
import type { Logger } from "pino";
import {
  BadPayloadError,
  BadSignatureError,
  SendError,
  type DeliveryReceipt,
  type InboundSMS,
  type OutboundSMS,
  type RawRequest,
  type SMSInbound,
  type SendErrorKind,
  type SendResult,
  type Sender,
  type StatusQuerier,
} from "./channel";

export const MOCK = "mock";

export interface SentMessage extends OutboundSMS {
  providerMessageId: string;
  at: Date;
}

export class MockGateway implements SMSInbound, Sender, StatusQuerier {
  readonly name = MOCK;
  readonly sent: SentMessage[] = [];
  private readonly byRef = new Map<string, string>();
  private readonly failNext: SendErrorKind[] = [];
  dlrDelayMs = 300;
  now = () => Date.now();

  constructor(
    private readonly secret: string,
    private readonly dlrUrl: string,
    private readonly log?: Logger,
  ) {}

  private check(req: RawRequest) {
    const err = verify(this.secret, req.header("X-Webhook-Signature"), req.header("X-Webhook-Timestamp"), req.body, Math.floor(this.now() / 1000));
    if (err) throw new BadSignatureError(err);
  }

  private json(req: RawRequest): Record<string, unknown> {
    try {
      return JSON.parse(req.body) as Record<string, unknown>;
    } catch {
      throw new BadPayloadError("not JSON");
    }
  }

  parseInbound(req: RawRequest): InboundSMS[] {
    this.check(req);
    const p = this.json(req);
    if (typeof p.message_id !== "string" || !p.message_id || typeof p.from !== "string" || !p.from) {
      throw new BadPayloadError("need message_id, from, text");
    }
    const at = typeof p.received_at === "string" && !Number.isNaN(Date.parse(p.received_at)) ? new Date(p.received_at) : new Date(this.now());
    return [
      {
        provider: MOCK,
        providerEventId: p.message_id,
        serviceId: typeof p.to === "string" ? p.to : "",
        from: p.from,
        body: typeof p.text === "string" ? p.text : "",
        receivedAt: at,
        environment: "sandbox",
      },
    ];
  }

  parseStatus(req: RawRequest): DeliveryReceipt[] {
    this.check(req);
    const p = this.json(req);
    if (typeof p.message_id !== "string" || !p.message_id || typeof p.status !== "string" || !p.status) {
      throw new BadPayloadError("need message_id and status");
    }
    return [
      {
        provider: MOCK,
        providerMessageId: p.message_id,
        status: p.status.toLowerCase(),
        errorCode: typeof p.error_code === "string" ? p.error_code : undefined,
        at: new Date(this.now()),
      },
    ];
  }

  /** Captures the message and, if configured, posts a signed delivery receipt back shortly afterwards. */
  async send(msg: OutboundSMS): Promise<SendResult> {
    const kind = this.failNext.shift();
    if (kind) throw new SendError(kind, "mock injected failure");
    const id = "mock-" + randomBytes(8).toString("hex");
    this.sent.push({ ...msg, providerMessageId: id, at: new Date(this.now()) });
    this.byRef.set(msg.clientRef, id);
    if (this.dlrUrl) setTimeout(() => void this.postReceipt(id), this.dlrDelayMs);
    return { providerMessageId: id };
  }

  async queryStatus(clientRef: string): Promise<{ receipt?: DeliveryReceipt; found: boolean }> {
    const id = this.byRef.get(clientRef);
    if (!id) return { found: false };
    return { found: true, receipt: { provider: MOCK, providerMessageId: id, status: "submitted", at: new Date(this.now()) } };
  }

  /** Makes the next send calls fail with the given kinds (tests). */
  failWith(...kinds: SendErrorKind[]) {
    this.failNext.push(...kinds);
  }

  /** Simulates a provider that accepted a message whose response was lost (reconciliation tests). */
  recordAcceptedWithoutReply(clientRef: string) {
    this.byRef.set(clientRef, "mock-" + randomBytes(8).toString("hex"));
  }

  private async postReceipt(providerMessageId: string) {
    const body = JSON.stringify({ message_id: providerMessageId, status: "delivered" });
    try {
      await fetch(this.dlrUrl, { method: "POST", headers: signedHeaders(this.secret, body), body, signal: AbortSignal.timeout(5000) });
    } catch (err) {
      this.log?.warn({ err: (err as Error).message }, "mock delivery receipt failed");
    }
  }
}
