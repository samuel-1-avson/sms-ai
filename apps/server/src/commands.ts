// Keyword commands handled before any AI call (plan §6.2): HELP, NEW, MORE,
// STOP, START, CODE <VOUCHER> and DELETE.

export type CommandKind = "HELP" | "NEW" | "MORE" | "STOP" | "START" | "CODE" | "DELETE";

export interface Command {
  kind: CommandKind | null;
  arg?: string;
}

const reCode = /^CODE[\s:#-]*([A-Z0-9-]{3,24})$/;
const reTrailing = /[\s.!?]+$/;

const keywords: Record<string, CommandKind> = {
  HELP: "HELP", "?": "HELP", INFO: "HELP", MENU: "HELP",
  NEW: "NEW", RESET: "NEW", "NEW CHAT": "NEW",
  MORE: "MORE", M: "MORE",
  STOP: "STOP", STOPALL: "STOP", UNSUBSCRIBE: "STOP", CANCEL: "STOP", OPTOUT: "STOP", "OPT OUT": "STOP",
  START: "START", UNSTOP: "START", SUBSCRIBE: "START",
  DELETE: "DELETE", "DELETE MY DATA": "DELETE", "FORGET ME": "DELETE",
};

/** Classifies a whole message. Commands must be the entire message, so "more examples of verbs" is a question. */
export function parseCommand(body: string): Command {
  let s = body.trim().toUpperCase().split(/\s+/).filter(Boolean).join(" ");
  if (s !== "?") s = s.replace(reTrailing, "");
  const kind = Object.prototype.hasOwnProperty.call(keywords, s) ? keywords[s] : undefined;
  if (kind) return { kind };
  const m = reCode.exec(s);
  if (m) return { kind: "CODE", arg: m[1] };
  return { kind: null };
}

// User-facing texts. Every one must stay within a single GSM-7 segment
// (enforced by tests).
export const Texts = {
  help:
    "SMS AI Ghana: text any question. MORE=rest, NEW=new chat, CODE x=study pass, STOP=opt out. " +
    "AI can be wrong. Chats kept 30 days. SMS rates may apply.",
  newChat: "New chat started. Ask me anything.",
  noMore: "Nothing more for your last answer. Send a new question anytime.",
  stop: "You will get no more messages from SMS AI Ghana. Text START to rejoin.",
  start: "Welcome to SMS AI Ghana! Text any question. Text HELP for commands.",
  deleted: "Your chat history has been deleted. Text any question to start again.",
  codeInvalid: "That code is not valid. Check it and try again, e.g. CODE STEMGH.",
  codeExpired: "That code has expired or its pool is used up. Ask your school or sponsor for a new one.",
  codeUsed: "You have already used that code on this number.",
  codeTooMany: "Too many wrong codes today. Please try again tomorrow.",
  busy: "SMS AI is busy right now. Please try again in a few minutes.",
  failed: "Sorry, I could not answer that right now. Please try again shortly.",
} as const;

/** The one-time daily-cap notice (plan §5.4). */
export function limitReachedText(used: number, limit: number): string {
  return `Daily limit reached (${used}/${limit}). Reply tomorrow or text CODE <voucher> if you have a school/study pass.`;
}

/** Confirms a voucher redemption. */
export function codeAcceptedText(granted: number, balance: number): string {
  return `Code accepted: ${granted} extra questions added. Bonus balance: ${balance}.`;
}
