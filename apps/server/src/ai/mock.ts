// A deterministic stand-in for DeepInfra for local development, the
// simulator and tests (LLM_PROVIDER=mock).
//
// Behaviour hooks, matched case-insensitively in the latest user message:
//   "slow"  -> waits slowLatencyMs
//   "long"  -> returns a ~300 character answer (exercises MORE)
//   "empty" -> returns empty content (exercises the reasoning-token guard)
//   "fail"  -> returns an upstream error
import { AIError, emptyResult, type ChatModel, type ChatRequest, type ChatResult } from "./model";

export class MockModel implements ChatModel {
  slowLatencyMs = 4500;
  calls = 0;
  lastRequest: ChatRequest | null = null;
  /** Overrides the canned behaviour when set (tests). */
  respond: ((req: ChatRequest) => string | Promise<string>) | null = null;

  constructor(public latencyMs: number) {}

  async complete(req: ChatRequest, signal?: AbortSignal): Promise<ChatResult> {
    this.calls++;
    this.lastRequest = req;

    let question = "";
    let history = 0;
    for (let i = req.messages.length - 1; i >= 0; i--) {
      if (req.messages[i]!.role !== "user") continue;
      if (question === "") question = req.messages[i]!.content;
      else history++;
    }
    const lower = question.toLowerCase();

    const start = Date.now();
    await sleep(lower.includes("slow") ? this.slowLatencyMs : this.latencyMs, signal).catch(() => {
      const reason = signal?.reason;
      if (reason instanceof DOMException && reason.name === "TimeoutError") {
        throw new AIError("timeout", "mock timed out", emptyResult(Date.now() - start));
      }
      throw new AIError("canceled", "mock canceled", emptyResult(Date.now() - start));
    });

    let input = 0;
    for (const m of req.messages) input += Math.floor(m.content.length / 4) + 4;
    const res: ChatResult = { ...emptyResult(Date.now() - start), model: req.model, inputTokens: input, finishReason: "stop" };

    let content: string;
    if (this.respond) {
      content = await this.respond(req);
    } else if (lower.includes("fail")) {
      throw new AIError("upstream", "mock failure", res);
    } else if (lower.includes("empty")) {
      throw new AIError("empty", "empty response (mock)", {
        ...res,
        finishReason: "length",
        reasoningTokens: req.maxOutputTokens,
        outputTokens: req.maxOutputTokens,
      });
    } else if (lower.includes("photosynthesis")) {
      // Curly quotes and an em dash on purpose: the GSM-7 sanitizer must fix them.
      content = "Sure! Photosynthesis is how plants use sunlight, water and carbon dioxide to make food — and it’s why we have oxygen.";
    } else if (lower.includes("sunlight") && history > 0) {
      content = "Sunlight supplies the energy plants need to turn water and carbon dioxide into sugars.";
    } else if (lower.includes("long")) {
      content = "Here is the answer: " + "This mock answer is deliberately long so that the MORE buffer is exercised. ".repeat(4);
    } else {
      const q = [...question];
      const short = q.length <= 60 ? question : q.slice(0, 60).join("") + "...";
      content = `Mock answer (${history} earlier turns): you asked "${short}". Set LLM_PROVIDER=deepinfra for real answers.`;
    }
    return { ...res, content, outputTokens: Math.floor(content.length / 4) + 1 };
  }
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal!.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
