// The MORE pager: sentence-aware splitting of an answer into single-segment
// pages (plan §6.2).
import { GSM7_SINGLE, UCS2_SINGLE, measure, unitCost, type Encoding } from "./gsm7";

/** Per-page capacities in encoding units. */
export interface Limits {
  gsm7: number;
  ucs2: number;
}

/** Keeps every page to exactly one billable SMS segment. */
export const SMS_LIMITS: Limits = { gsm7: GSM7_SINGLE, ucs2: UCS2_SINGLE };

export function limitFor(lim: Limits, enc: Encoding): number {
  return enc === "ucs2" ? lim.ucs2 : lim.gsm7;
}

/** Appended to a page that has a continuation. */
export const SMS_MORE_SUFFIX = " ..Txt MORE";

type CutKind = "all" | "sentence" | "word" | "hard";

// Finds the index at which to cut text so the head fits in budget units,
// preferring the end of a sentence, then a word boundary, then a hard cut.
function cutIndex(text: string, enc: Encoding, budget: number): [number, CutKind] {
  if (budget <= 0) return [0, "hard"];
  let used = 0;
  let lastFit = 0;
  let lastSpace = -1;
  let spaceUnits = 0;
  let lastSentence = -1;
  let sentenceUnits = 0;
  let prevTerminal = false;
  let i = 0;
  for (const ch of text) {
    const c = unitCost(enc, ch);
    if (used + c > budget) break;
    if (ch === " ") {
      lastSpace = i;
      spaceUnits = used;
      if (prevTerminal) {
        lastSentence = i;
        sentenceUnits = used;
      }
    }
    used += c;
    lastFit = i + ch.length;
    prevTerminal = ch === "." || ch === "!" || ch === "?";
    i += ch.length;
  }
  if (lastFit >= text.length) return [text.length, "all"];
  if (text[lastFit] === " ") {
    lastSpace = lastFit;
    spaceUnits = used;
    if (prevTerminal) {
      lastSentence = lastFit;
      sentenceUnits = used;
    }
  }
  if (lastSentence > 0 && sentenceUnits * 2 >= budget) return [lastSentence, "sentence"];
  if (lastSpace > 0 && spaceUnits * 10 >= budget * 3) return [lastSpace, "word"];
  return [lastFit, "hard"];
}

function unitsOf(enc: Encoding, s: string): number {
  let n = 0;
  for (const ch of s) n += unitCost(enc, ch);
  return n;
}

function trimRightChars(s: string, chars: string): string {
  let end = s.length;
  while (end > 0 && chars.includes(s[end - 1]!)) end--;
  return s.slice(0, end);
}

/** Whether text fits on one page. */
export function fits(text: string, lim: Limits): boolean {
  const [enc, n] = measure(text);
  return n <= limitFor(lim, enc);
}

/** Returns the first page (with suffix when there is a continuation) and the remaining text. The first page always fits. */
export function splitFirst(text: string, lim: Limits, suffix: string): [string, string] {
  text = text.trim();
  const [enc, n] = measure(text);
  const limit = limitFor(lim, enc);
  if (n <= limit) return [text, ""];
  const budget = limit - unitsOf(enc, suffix);
  if (budget < limit / 3) return [truncate(text, lim), ""];
  const [idx, kind] = cutIndex(text, enc, budget);
  let head = text.slice(0, idx).trim();
  if (kind === "word") {
    // Mid-sentence: drop dangling punctuation so the page reads cleanly.
    head = trimRightChars(head, " ,;:-");
  }
  if (head === "") return [truncate(text, lim), ""];
  return [head + suffix, text.slice(idx).trim()];
}

/** Shortens text to one page, ending on a full sentence when one is long enough, otherwise on a word boundary plus "...". */
export function truncate(text: string, lim: Limits): string {
  text = text.trim();
  const [enc, n] = measure(text);
  const limit = limitFor(lim, enc);
  if (n <= limit) return text;
  const [sIdx, kind] = cutIndex(text, enc, limit);
  if (kind === "sentence") return text.slice(0, sIdx).trim();
  const [idx] = cutIndex(text, enc, limit - 3);
  return trimRightChars(text.slice(0, idx), " ,;:-") + "...";
}

/** Splits text into at most maxPages pages; every page but the last ends with suffix. */
export function paginate(text: string, lim: Limits, suffix: string, maxPages: number): string[] {
  if (maxPages < 1) maxPages = 1;
  const pages: string[] = [];
  let rest = text.trim();
  while (pages.length < maxPages - 1) {
    const [first, r] = splitFirst(rest, lim, suffix);
    pages.push(first);
    if (r === "") return pages;
    rest = r;
  }
  pages.push(truncate(rest, lim));
  return pages;
}
