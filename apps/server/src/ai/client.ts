// Orchestration around a ChatModel: the reasoning-token guard, one fallback
// model attempt, a circuit breaker and cost estimation.
import type { ModelPrice } from "../config";
import { AIError, isAIError, isReasoningModel, isRetryable, type ChatMessage, type ChatModel, type ChatRequest, type ChatResult } from "./model";

/** Leaves room for ~150 reasoning tokens plus a short visible answer (plan §7.2 rule 1). */
export const MIN_REASONING_MAX_TOKENS = 350;

/** One model attempt, for the model_calls ledger. */
export interface CallRecord {
  model: string;
  status: "ok" | "empty" | "error";
  inputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  costUsd: number;
  latencyMs: number;
  error?: string;
}

export interface Outcome {
  text: string;
  model: string;
  calls: CallRecord[];
}

/** Per-attempt telemetry (implemented by the metrics module). */
export interface AIObserver {
  observeAICall(model: string, status: string, latencyMs: number): void;
  observeAIEmptyReasoning(model: string): void;
}

export interface ClientOptions {
  primary: string;
  fallback?: string;
  reasoningEffort?: string;
  maxOutputTokens: number;
  temperature: number;
  prices?: Map<string, ModelPrice>;
  breaker?: Breaker;
  observer?: AIObserver;
}

/** Thrown by Client.answer; carries the attempts made so they can be recorded. */
export class AnswerError extends Error {
  constructor(
    /** The last attempt's error (usually an AIError). */
    readonly error: unknown,
    readonly calls: CallRecord[],
  ) {
    super(error instanceof Error ? error.message : String(error));
  }
}

export class Client {
  readonly breaker: Breaker;

  constructor(
    private readonly model: ChatModel,
    private readonly opts: ClientOptions,
  ) {
    this.breaker = opts.breaker ?? new Breaker(5, 30_000, 5 * 60_000);
  }

  /** Asks the primary model, then the fallback once if the primary returned empty text or a retryable error and time remains. */
  async answer(messages: ChatMessage[], signal?: AbortSignal): Promise<Outcome> {
    const calls: CallRecord[] = [];
    try {
      this.breaker.allow();
    } catch (err) {
      throw new AnswerError(err, calls);
    }
    const models = [this.opts.primary];
    if (this.opts.fallback && this.opts.fallback !== this.opts.primary) models.push(this.opts.fallback);
    let lastErr: unknown = null;
    for (let i = 0; i < models.length; i++) {
      if (i > 0 && !isRetryable(lastErr)) break;
      if (signal?.aborted) break;
      const model = models[i]!;
      let res: ChatResult | undefined;
      let err: unknown = null;
      try {
        res = await this.model.complete(this.request(model, messages), signal);
      } catch (e) {
        err = e;
        res = isAIError(e) ? e.result : undefined;
      }
      const rec = this.record(model, res, err);
      calls.push(rec);
      this.opts.observer?.observeAICall(model, rec.status, rec.latencyMs);
      if (isAIError(err, "empty") && isReasoningModel(model)) this.opts.observer?.observeAIEmptyReasoning(model);
      // A deadline imposed by the caller says nothing about provider health.
      if (!signal?.aborted) this.breaker.record(err);
      if (err === null && res) return { text: res.content, model, calls };
      lastErr = err;
    }
    throw new AnswerError(lastErr ?? new AIError("timeout", "deadline exceeded before any attempt"), calls);
  }

  private request(model: string, messages: ChatMessage[]): ChatRequest {
    const req: ChatRequest = {
      messages,
      maxOutputTokens: this.opts.maxOutputTokens,
      model,
      temperature: this.opts.temperature,
    };
    if (isReasoningModel(model)) {
      req.reasoningEffort = this.opts.reasoningEffort || "low";
      req.maxOutputTokens = Math.max(req.maxOutputTokens, MIN_REASONING_MAX_TOKENS);
    }
    return req;
  }

  private record(model: string, res: ChatResult | undefined, err: unknown): CallRecord {
    const rec: CallRecord = {
      model,
      status: err === null ? "ok" : isAIError(err, "empty") ? "empty" : "error",
      inputTokens: res?.inputTokens ?? 0,
      outputTokens: res?.outputTokens ?? 0,
      reasoningTokens: res?.reasoningTokens ?? 0,
      costUsd: res?.estimatedCostUsd ?? 0,
      latencyMs: res?.latencyMs ?? 0,
    };
    if (err !== null) rec.error = err instanceof Error ? err.message : String(err);
    if (rec.costUsd <= 0) rec.costUsd = estimateCost(this.opts.prices, model, rec.inputTokens, rec.outputTokens);
    return rec;
  }
}

// Planning prices in USD per 1M tokens; unknown models use a deliberately
// high price so spend caps err on the safe side. Override with LLM_PRICES.
const defaultPrices = new Map<string, ModelPrice>([["openai/gpt-oss-20b", { inputPerM: 0.03, outputPerM: 0.14 }]]);
const fallbackPrice: ModelPrice = { inputPerM: 0.6, outputPerM: 0.6 };

export function estimateCost(prices: Map<string, ModelPrice> | undefined, model: string, input: number, output: number): number {
  const p = prices?.get(model) ?? defaultPrices.get(model) ?? fallbackPrice;
  return (input / 1e6) * p.inputPerM + (output / 1e6) * p.outputPerM;
}

/**
 * A small in-process circuit breaker. Credential or credit failures open it
 * for a long cooldown; bursts of transient failures open it briefly.
 */
export class Breaker {
  private failures = 0;
  private openUntil = 0;
  private reason = "";

  constructor(
    private readonly threshold: number,
    private readonly shortCooldownMs: number,
    private readonly longCooldownMs: number,
    private readonly now: () => number = Date.now,
  ) {}

  allow(): void {
    if (this.now() < this.openUntil) throw new AIError("circuit_open", "circuit breaker open");
  }

  record(err: unknown): void {
    if (err === null || err === undefined) {
      this.failures = 0;
    } else if (isAIError(err, "quota") || isAIError(err, "unauthorized")) {
      this.open(this.longCooldownMs, err);
    } else if (isAIError(err, "canceled") || isAIError(err, "bad_request") || isAIError(err, "empty")) {
      // Caller cancelled, a request-specific problem, or an empty reasoning
      // answer (handled by the fallback model): not a provider outage.
    } else if (++this.failures >= this.threshold) {
      this.open(this.shortCooldownMs, err);
    }
  }

  private open(ms: number, err: unknown) {
    this.openUntil = this.now() + ms;
    this.reason = err instanceof Error ? err.message : String(err);
    this.failures = 0;
  }

  /** "closed" or "open: <reason>" for the admin API. */
  state(): string {
    return this.now() < this.openUntil ? "open: " + this.reason.replace(/^ai: /, "") : "closed";
  }
}
