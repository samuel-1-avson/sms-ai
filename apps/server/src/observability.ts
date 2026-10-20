// Structured logging and the Prometheus metrics from plan §13. Metrics
// never carry message content or phone numbers.
import { pino, type Logger } from "pino";
import { Counter, Gauge, Histogram, Registry, collectDefaultMetrics } from "prom-client";
import type { AIObserver } from "./ai";

export function newLogger(level: string): Logger {
  return pino({ level: ["debug", "info", "warn", "error"].includes(level) ? level : "info", base: undefined });
}

export class Metrics implements AIObserver {
  readonly registry = new Registry();

  readonly channelInbound = this.counter("channel_inbound_total", "Inbound SMS messages.", ["provider", "carrier"]);
  readonly groundingHits = this.counter("grounding_store_hits_total", "Direct answers from the Local Ghana Grounding Store.", ["category"]);
  readonly groundingMisses = this.counter("grounding_store_misses_total", "Questions with no direct grounding answer.");
  readonly smsInboundCoalesced = this.counter("sms_inbound_coalesced_messages_total", "Inbound texts merged into an already pending job.");
  readonly smsMoreBufferServed = this.counter("sms_more_buffer_served_total", "MORE continuations served from the overflow buffer.");
  readonly smsOutboundSubmitted = this.counter("sms_outbound_submitted_total", "Outbound SMS accepted by the provider.", ["provider", "encoding", "segments"]);
  readonly smsOutboundFailed = this.counter("sms_outbound_failed_total", "Outbound SMS that failed or have an unknown outcome.", ["provider", "reason"]);
  readonly aiRequests = this.counter("ai_requests_total", "Model calls by outcome.", ["model", "status"]);
  readonly aiEmptyReasoning = this.counter("ai_empty_reasoning_response_total", "Reasoning-model calls that returned no visible text (must stay 0).", ["model"]);
  readonly aiLatency = new Histogram({
    name: "ai_request_latency_seconds",
    help: "Model call latency.",
    labelNames: ["model"],
    buckets: [0.25, 0.5, 1, 1.5, 2, 2.5, 3.2, 4, 6, 8, 12, 20],
    registers: [this.registry],
  });
  readonly dailySpendUsd = this.gauge("daily_spend_usd_total", "Estimated spend today (SMS segments + AI tokens).");
  readonly dailySpendCapUsd = this.gauge("daily_spend_cap_usd", "Configured global daily spend cap.");
  readonly voucherSegmentsRedeemed = this.counter("voucher_segments_redeemed_total", "Bonus credits granted by voucher redemptions.");
  readonly webhookRejected = this.counter("webhook_rejected_total", "Webhooks rejected before processing.", ["provider", "reason"]);
  readonly duplicateInbound = this.counter("sms_inbound_duplicate_total", "Repeated inbound webhooks ignored by idempotency.");
  readonly jobsProcessed = this.counter("jobs_processed_total", "Worker jobs by kind and result.", ["kind", "result"]);
  readonly dailyLimitReached = this.counter("daily_limit_reached_total", "Questions refused because the per-number daily cap was reached.");

  readonly gatewayInboundDropped = this.counter("gateway_inbound_dropped_total", "Texts the phone forwarded that are never answered.", ["reason"]);
  readonly gatewayOutboxExpired = this.counter("gateway_outbox_expired_total", "Replies that waited too long for the phone and were given up.");
  readonly gatewayOutboxStale = this.counter("gateway_outbox_stale_total", "Replies the phone acknowledged but never reported on.");
  readonly gatewayLastSeen = this.gauge("gateway_last_seen_timestamp_seconds", "Unix time of the last heartbeat from the gateway phone.");
  readonly gatewayQueue = this.gauge("gateway_phone_queue", "Items waiting on the phone, from its heartbeat.", ["queue"]);
  readonly gatewayBattery = this.gauge("gateway_phone_battery_percent", "Gateway phone battery level.");

  constructor() {
    collectDefaultMetrics({ register: this.registry });
  }

  private counter(name: string, help: string, labelNames: string[] = []) {
    return new Counter({ name, help, labelNames, registers: [this.registry] });
  }

  private gauge(name: string, help: string, labelNames: string[] = []) {
    return new Gauge({ name, help, labelNames, registers: [this.registry] });
  }

  observeAICall(model: string, status: string, latencyMs: number): void {
    this.aiRequests.inc({ model, status });
    this.aiLatency.observe({ model }, latencyMs / 1000);
  }

  observeAIEmptyReasoning(model: string): void {
    this.aiEmptyReasoning.inc({ model });
  }

  observeSubmitted(provider: string, encoding: string, segments: number): void {
    this.smsOutboundSubmitted.inc({ provider, encoding, segments: String(segments) });
  }
}
