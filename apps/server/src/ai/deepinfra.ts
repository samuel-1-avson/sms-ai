// DeepInfra's OpenAI-compatible chat completions API (plan §7.2):
// POST {base}/chat/completions with a Bearer token.
import { AIError, emptyResult, type ChatModel, type ChatRequest, type ChatResult } from "./model";

interface ChatResponse {
  model?: string;
  choices?: { message?: { content?: string | null }; finish_reason?: string }[];
  usage?: {
    prompt_tokens?: number;
    completion_tokens?: number;
    estimated_cost?: number;
    completion_tokens_details?: { reasoning_tokens?: number } | null;
  };
}

// A backstop when the caller passes no deadline.
const BACKSTOP_MS = 60_000;

export class DeepInfra implements ChatModel {
  private readonly baseUrl: string;

  constructor(
    baseUrl: string,
    private readonly apiKey: string,
  ) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
  }

  async complete(req: ChatRequest, signal?: AbortSignal): Promise<ChatResult> {
    const body = JSON.stringify({
      model: req.model,
      messages: req.messages,
      max_tokens: req.maxOutputTokens || undefined,
      temperature: req.temperature,
      reasoning_effort: req.reasoningEffort || undefined,
      stream: false,
    });
    const backstop = AbortSignal.timeout(BACKSTOP_MS);
    const combined = signal ? AbortSignal.any([signal, backstop]) : backstop;
    const start = performance.now();
    const elapsed = () => Math.round(performance.now() - start);

    let resp: Response;
    let raw: string;
    try {
      resp = await fetch(this.baseUrl + "/chat/completions", {
        method: "POST",
        headers: { Authorization: "Bearer " + this.apiKey, "Content-Type": "application/json" },
        body,
        signal: combined,
      });
      raw = await resp.text();
    } catch (err) {
      const latency = elapsed();
      if (combined.aborted) {
        const reason = signal?.aborted ? signal.reason : backstop.reason;
        if (reason instanceof DOMException && reason.name === "TimeoutError") {
          throw new AIError("timeout", `request timed out after ${latency} ms`, emptyResult(latency));
        }
        throw new AIError("canceled", "request canceled", emptyResult(latency));
      }
      throw new AIError("upstream", (err as Error).message, emptyResult(latency));
    }
    const latencyMs = elapsed();
    if (resp.status !== 200) throw statusError(resp.status, raw, latencyMs);

    let parsed: ChatResponse;
    try {
      parsed = JSON.parse(raw) as ChatResponse;
    } catch (err) {
      throw new AIError("upstream", `decoding response: ${(err as Error).message}`, emptyResult(latencyMs));
    }
    const choice = parsed.choices?.[0];
    const res: ChatResult = {
      content: (choice?.message?.content ?? "").trim(),
      model: parsed.model || req.model,
      finishReason: choice?.finish_reason ?? "",
      inputTokens: parsed.usage?.prompt_tokens ?? 0,
      outputTokens: parsed.usage?.completion_tokens ?? 0,
      reasoningTokens: parsed.usage?.completion_tokens_details?.reasoning_tokens ?? 0,
      estimatedCostUsd: parsed.usage?.estimated_cost ?? 0,
      latencyMs,
    };
    if (res.content === "") {
      // The Reasoning-Token Trap: tokens were spent but nothing visible came back.
      throw new AIError("empty", `empty response (finish_reason="${res.finishReason}", reasoning_tokens=${res.reasoningTokens})`, res);
    }
    return res;
  }
}

function statusError(code: number, body: string, latencyMs: number): AIError {
  const snippet = body.trim().slice(0, 200);
  const r = emptyResult(latencyMs);
  if (code === 401 || code === 403) return new AIError("unauthorized", `HTTP ${code}`, r);
  if (code === 402) return new AIError("quota", `HTTP ${code}`, r);
  if (code === 429) return new AIError("rate_limited", `HTTP ${code}`, r);
  if (code === 408 || code === 504) return new AIError("timeout", `HTTP ${code}`, r);
  if (code >= 500) return new AIError("upstream", `HTTP ${code}: ${snippet}`, r);
  return new AIError("bad_request", `HTTP ${code}: ${snippet}`, r);
}
