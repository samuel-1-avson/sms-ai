// Provider-neutral contracts between SMS gateways and the domain (plan §4.2,
// §9). The domain never sees provider-specific payloads.

/** The normalized inbound event (plan §9). */
export interface InboundSMS {
  provider: string;
  providerEventId: string;
  serviceId: string;
  from: string;
  body: string;
  receivedAt: Date;
  environment: string;
}

/** A normalized delivery report. */
export interface DeliveryReceipt {
  provider: string;
  providerMessageId: string;
  /** submitted | sent | delivered | failed | expired | rejected | unknown */
  status: string;
  errorCode?: string;
  at: Date;
}

export interface OutboundSMS {
  to: string;
  senderId: string;
  body: string;
  encoding: string;
  segments: number;
  /** Our idempotency key (the message id). */
  clientRef: string;
}

export interface SendResult {
  providerMessageId: string;
}

/**
 * Tells the dispatcher whether resending is safe:
 * retryable = the provider definitely did not accept the message;
 * permanent = rejected (bad number); do not retry;
 * uncertain = the request may have been accepted; reconcile before any retry (plan §5.4).
 */
export type SendErrorKind = "retryable" | "permanent" | "uncertain";

export class SendError extends Error {
  constructor(
    readonly kind: SendErrorKind,
    message: string,
  ) {
    super(`send error (${kind}): ${message}`);
  }
}

/** Classifies any error thrown by Sender.send. */
export function sendErrorKindOf(err: unknown): SendErrorKind {
  if (err instanceof SendError) return err.kind;
  if (err instanceof DOMException && (err.name === "TimeoutError" || err.name === "AbortError")) return "uncertain";
  return "retryable";
}

export class BadSignatureError extends Error {
  constructor(detail = "") {
    super("channel: webhook signature invalid" + (detail ? `: ${detail}` : ""));
  }
}

export class BadPayloadError extends Error {
  constructor(detail: string) {
    super("channel: webhook payload invalid: " + detail);
  }
}

/** What an adapter needs from an HTTP request: headers and the raw body (signatures cover the exact bytes). */
export interface RawRequest {
  header(name: string): string | undefined;
  body: string;
}

/** Parses (and authenticates) inbound SMS and delivery webhooks. */
export interface SMSInbound {
  readonly name: string;
  parseInbound(req: RawRequest): InboundSMS[];
  parseStatus(req: RawRequest): DeliveryReceipt[];
}

/** Submits outbound SMS. */
export interface Sender {
  readonly name: string;
  send(msg: OutboundSMS, signal?: AbortSignal): Promise<SendResult>;
}

/** Providers with a query-by-reference API, used to reconcile uncertain submissions. */
export interface StatusQuerier {
  queryStatus(clientRef: string, providerMessageId: string): Promise<{ receipt?: DeliveryReceipt; found: boolean }>;
}

export const isStatusQuerier = (s: unknown): s is StatusQuerier =>
  typeof (s as Partial<StatusQuerier>)?.queryStatus === "function";

/** The configured providers by name. */
export class Registry {
  private readonly inbound = new Map<string, SMSInbound>();
  private readonly senders = new Map<string, Sender>();

  constructor(private readonly primarySender: string) {}

  addInbound(p: SMSInbound) {
    this.inbound.set(p.name, p);
  }

  addSender(s: Sender) {
    this.senders.set(s.name, s);
  }

  smsInbound(name: string): SMSInbound | undefined {
    return this.inbound.get(name);
  }

  /** A provider replies through itself; otherwise the primary sender is used. */
  senderFor(conversationProvider: string): Sender {
    const s = this.senders.get(conversationProvider) ?? this.senders.get(this.primarySender);
    if (!s) throw new Error(`channel: no SMS sender for "${conversationProvider}" and primary "${this.primarySender}" is not configured`);
    return s;
  }
}
