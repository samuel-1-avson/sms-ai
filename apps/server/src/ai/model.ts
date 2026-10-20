// The provider-neutral chat model contract (plan §7.1).

export interface ChatMessage {
  role: "system" | "user" | "assistant";
  content: string;
}

export interface ChatRequest {
  messages: ChatMessage[];
  maxOutputTokens: number;
  model: string;
  /** "low" for reasoning models, undefined otherwise. */
  reasoningEffort?: string;
  temperature: number;
}

export interface ChatResult {
  content: string;
  model: string;
  finishReason: string;
  inputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  /** Provider-reported cost when available. */
  estimatedCostUsd: number;
  latencyMs: number;
}

export const emptyResult = (latencyMs = 0): ChatResult => ({
  content: "",
  model: "",
  finishReason: "",
  inputTokens: 0,
  outputTokens: 0,
  reasoningTokens: 0,
  estimatedCostUsd: 0,
  latencyMs,
});

export type AIErrorCode =
  | "empty" // no visible text: a reasoning model spent the whole budget thinking (plan §7.2)
  | "timeout"
  | "rate_limited"
  | "quota" // provider quota or credit exhausted
  | "unauthorized"
  | "upstream"
  | "bad_request"
  | "circuit_open"
  | "canceled";

/** A model failure. `result` carries the tokens spent so they are still recorded. */
export class AIError extends Error {
  constructor(
    readonly code: AIErrorCode,
    message: string,
    readonly result: ChatResult = emptyResult(),
  ) {
    super(`ai: ${message}`);
  }
}

export const isAIError = (err: unknown, code?: AIErrorCode): err is AIError =>
  err instanceof AIError && (code === undefined || err.code === code);

/** Whether a later attempt may succeed. */
export function isRetryable(err: unknown): boolean {
  return err instanceof AIError && ["timeout", "rate_limited", "upstream", "empty", "circuit_open"].includes(err.code);
}

/** Implemented by provider adapters. Must throw AIError on failure; honours `signal` for deadlines. */
export interface ChatModel {
  complete(req: ChatRequest, signal?: AbortSignal): Promise<ChatResult>;
}

/** Models that emit hidden chain-of-thought tokens and need reasoning_effort plus a larger budget. */
export function isReasoningModel(model: string): boolean {
  const m = model.toLowerCase();
  return ["gpt-oss", "deepseek-r1", "qwq", "-thinking", "o1-", "o3-", "o4-"].some((marker) => m.includes(marker));
}
