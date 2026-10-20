// The phone <-> server protocol (/gateway/v1/*). Every request is signed
// with the device secret (see signing.ts); the server validates bodies with
// these schemas after the signature check.
import { z } from "zod";

export const GATEWAY_PROVIDER = "gateway";

/** Every route is a POST with a signed JSON body (the outbox long-poll sends {"wait": seconds}). */
export const GatewayPaths = {
  inbound: "/gateway/v1/inbound",
  outbox: "/gateway/v1/outbox",
  ack: "/gateway/v1/ack",
  status: "/gateway/v1/status",
  heartbeat: "/gateway/v1/heartbeat",
} as const;

/** Longest a long-poll for outbound messages may wait, in seconds. */
export const MAX_OUTBOX_WAIT_SECONDS = 25;
/** Largest batch either side sends in one request. */
export const MAX_BATCH = 50;

const id = z.string().min(8).max(64).regex(/^[A-Za-z0-9-]+$/);

/** One SMS received by the phone. `id` is a UUID made on the phone when it was saved locally, so retries reuse it. */
export const InboundSMS = z.object({
  id,
  from: z.string().min(1).max(32),
  body: z.string().max(4000),
  received_at: z.string().datetime({ offset: true }),
  sim: z.string().max(64).optional(),
});
export type InboundSMS = z.infer<typeof InboundSMS>;

export const InboundBatch = z.object({ messages: z.array(InboundSMS).min(1).max(MAX_BATCH) });
export type InboundBatch = z.infer<typeof InboundBatch>;

/** One SMS the phone must send. `id` is the server's message id. */
export const OutboundSMS = z.object({
  id,
  to: z.string().min(1).max(32),
  body: z.string().min(1).max(2000),
});
export type OutboundSMS = z.infer<typeof OutboundSMS>;

export const OutboxRequest = z.object({ wait: z.number().int().min(0).max(MAX_OUTBOX_WAIT_SECONDS).default(0) });
export type OutboxRequest = z.infer<typeof OutboxRequest>;

export const OutboxResponse = z.object({ messages: z.array(OutboundSMS).max(MAX_BATCH) });
export type OutboxResponse = z.infer<typeof OutboxResponse>;

/** The phone has saved these messages locally and now owns sending them. */
export const AckRequest = z.object({ ids: z.array(id).min(1).max(MAX_BATCH) });
export type AckRequest = z.infer<typeof AckRequest>;

export const SendStatus = z.enum(["sent", "delivered", "failed"]);
export type SendStatus = z.infer<typeof SendStatus>;

export const StatusUpdate = z.object({
  id,
  status: SendStatus,
  error: z.string().max(200).optional(),
  at: z.string().datetime({ offset: true }).optional(),
});
export type StatusUpdate = z.infer<typeof StatusUpdate>;

export const StatusBatch = z.object({ statuses: z.array(StatusUpdate).min(1).max(MAX_BATCH) });
export type StatusBatch = z.infer<typeof StatusBatch>;

export const Heartbeat = z.object({
  device_id: z.string().min(1).max(64),
  app_version: z.string().max(32),
  battery: z.number().min(0).max(100).nullable(),
  charging: z.boolean().nullable(),
  signal: z.number().int().min(0).max(4).nullable(),
  sim_label: z.string().max(64).nullable(),
  inbox_pending: z.number().int().min(0),
  outbox_pending: z.number().int().min(0),
  status_pending: z.number().int().min(0),
  paused: z.boolean(),
});
export type Heartbeat = z.infer<typeof Heartbeat>;

/** Contents of the pairing QR code printed by `admin gateway-pair`. */
export const Pairing = z.object({
  v: z.literal(1),
  url: z.string().url(),
  secret: z.string().min(24),
});
export type Pairing = z.infer<typeof Pairing>;
