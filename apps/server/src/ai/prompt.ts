// Assembles the model input: the hardened system prompt (plan §7.4),
// verified grounding facts, a bounded window of recent turns and the
// user's question.
import type { ChatMessage } from "./model";

/** Tightened after the 2026-10-17 benchmark: a reply normally fits one 160-character SMS. */
export const SYSTEM_PROMPT = `You are SMS AI Ghana, a concise, accurate text assistant for users in Ghana.
Every reply is sent as ONE text message of at most 150 characters (about 25 words). Count carefully.
Give the direct answer in one or two short sentences. Leave out greetings, background and examples unless asked.
Only when the user asks for steps or a worked solution may you go longer, up to 300 characters, still with the key result first.
Write plain ASCII only: straight quotes ' and ", hyphen -, x for times, / for divide, ^ for powers, GHS for cedis. No other symbols.
Never use markdown, bold, lists, line breaks, or emojis.
Use conversation history only to resolve follow-up pronouns or context.
Do not invent live prices, flight schedules, news, exchange rates, or bookings; state clearly when live info is unavailable.
Do not name specific companies, agencies, websites or phone numbers unless they are official and you are sure they exist; give general guidance instead.
For health, legal, or financial topics, give general educational facts and advise consulting a qualified professional.
Decline harmful or illegal requests briefly. Never ask for PINs, MoMo OTPs, passwords, or Ghana Card numbers.`;

export interface Turn {
  role: "user" | "assistant";
  content: string;
}

export interface PromptInput {
  question: string;
  /** Oldest first. */
  history: Turn[];
  facts: string[];
  now?: Date;
  maxInputTokens: number;
}

/** A conservative chars/4 approximation; exact counts come back from the provider. */
export function estimateTokens(s: string): number {
  return Math.floor((Buffer.byteLength(s, "utf8") + 3) / 4) + 4;
}

const MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];

/** Builds the chat messages, dropping the oldest history turns until the estimate fits maxInputTokens. */
export function buildPrompt(input: PromptInput): ChatMessage[] {
  let sys = SYSTEM_PROMPT;
  if (input.now) {
    const d = input.now;
    sys += `\nToday's date is ${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}.`;
  }
  for (const f of input.facts) sys += "\nVerified Ghana Fact: " + f.trim();
  const system: ChatMessage = { role: "system", content: sys };
  const question: ChatMessage = { role: "user", content: input.question };

  const budget = input.maxInputTokens > 0 ? input.maxInputTokens : 800;
  let history = input.history;
  let total = estimateTokens(system.content) + estimateTokens(question.content);
  for (const t of history) total += estimateTokens(t.content);
  while (history.length > 0 && total > budget) {
    total -= estimateTokens(history[0]!.content);
    history = history.slice(1);
  }
  // Never start the window on an assistant turn without its question.
  while (history.length > 0 && history[0]!.role !== "user") history = history.slice(1);

  return [system, ...history.map((t) => ({ role: t.role, content: t.content })), question];
}
