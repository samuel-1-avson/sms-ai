// The Android phone gateway. People text the phone's SIM; the app forwards
// each SMS to /gateway/v1/inbound and sends our replies from the same SIM.
//
// Sending is a database insert into gateway_outbox that the phone
// long-polls, so it never has an uncertain outcome here. The phone's local
// store, keyed by message id, is what makes delivery exactly-once.
import {
  GATEWAY_PROVIDER,
  HEADER_SIGNATURE,
  HEADER_TIMESTAMP,
  InboundBatch,
  StatusBatch,
  verify,
  type StatusUpdate,
} from "@smsai/shared";
import type { z } from "zod";
import { normalize, tryNormalize } from "../msisdn";
import { GATEWAY_CHANNEL, type Store } from "../repository/store";
import {
  BadPayloadError,
  BadSignatureError,
  type DeliveryReceipt,
  type InboundSMS,
  type OutboundSMS,
  type RawRequest,
  type SMSInbound,
  type SendResult,
  type Sender,
} from "./channel";

export const GATEWAY = GATEWAY_PROVIDER;

/** Why an inbound text was not passed to the engine. */
export type DropReason = "non_numeric_sender" | "own_number";

export interface GatewayOptions {
  secrets: string[];
  serviceId: string;
  /** The gateway SIM's own number; texts from it are dropped (loop guard). */
  ownNumber: string;
  now?: () => number;
}

/** Verifies a phone request against any configured device secret (two while rotating). */
export function verifyDevice(secrets: string[], req: RawRequest, nowSeconds: number): void {
  let reason = "missing";
  for (const secret of secrets) {
    const err = verify(secret, req.header(HEADER_SIGNATURE), req.header(HEADER_TIMESTAMP), req.body, nowSeconds);
    if (err === null) return;
    reason = err;
  }
  throw new BadSignatureError(reason);
}

export function parseBody<T extends z.ZodTypeAny>(schema: T, body: string): z.infer<T> {
  let json: unknown;
  try {
    json = JSON.parse(body);
  } catch {
    throw new BadPayloadError("not JSON");
  }
  const res = schema.safeParse(json);
  if (!res.success) throw new BadPayloadError(res.error.issues.map((i) => `${i.path.join(".")}: ${i.message}`).join("; "));
  return res.data;
}

export class GatewayChannel implements SMSInbound, Sender {
  readonly name = GATEWAY;
  private readonly own: string | null;
  private readonly now: () => number;
  /** Filled by parseInbound with texts dropped before the engine, for metrics. */
  lastDropped: DropReason[] = [];

  constructor(
    private readonly store: Store,
    private readonly opts: GatewayOptions,
  ) {
    this.own = opts.ownNumber ? tryNormalize(opts.ownNumber) : null;
    this.now = opts.now ?? Date.now;
  }

  authenticate(req: RawRequest): void {
    verifyDevice(this.opts.secrets, req, Math.floor(this.now() / 1000));
  }

  /**
   * Texts from alphanumeric senders (network promos like "MTN"), short codes
   * and the gateway's own number are dropped here and acknowledged, so the
   * phone never retries them and nothing ever replies to them.
   */
  parseInbound(req: RawRequest): InboundSMS[] {
    this.authenticate(req);
    const batch = parseBody(InboundBatch, req.body);
    this.lastDropped = [];
    const out: InboundSMS[] = [];
    for (const m of batch.messages) {
      const from = tryNormalize(m.from);
      if (from === null || !/^\+\d{8,15}$/.test(from)) {
        this.lastDropped.push("non_numeric_sender");
        continue;
      }
      if (from === this.own) {
        this.lastDropped.push("own_number");
        continue;
      }
      out.push({
        provider: GATEWAY,
        providerEventId: m.id,
        serviceId: this.opts.serviceId,
        from,
        body: m.body,
        receivedAt: new Date(m.received_at),
        environment: "live",
      });
    }
    return out;
  }

  /** Maps phone send results to receipts; the caller also records them in the outbox (statusUpdates). */
  parseStatus(req: RawRequest): DeliveryReceipt[] {
    return this.statusUpdates(req).map((s) => ({
      provider: GATEWAY,
      providerMessageId: s.id,
      status: s.status,
      errorCode: s.error,
      at: s.at ? new Date(s.at) : new Date(this.now()),
    }));
  }

  statusUpdates(req: RawRequest): StatusUpdate[] {
    this.authenticate(req);
    return parseBody(StatusBatch, req.body).statuses;
  }

  async send(msg: OutboundSMS): Promise<SendResult> {
    const to = normalize(msg.to);
    await this.store.q().gatewayEnqueue(msg.clientRef, to, msg.body, msg.segments);
    await this.store.q().notify(GATEWAY_CHANNEL);
    return { providerMessageId: msg.clientRef };
  }
}
