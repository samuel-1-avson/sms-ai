// Deterministic GSM-7 sanitizer: turns raw model output into compact
// single-line text that stays in GSM-7 (plan §6.2 "The Homoglyph Trap").
import { isGSM7 } from "./gsm7";

export interface SanitizeOptions {
  /**
   * Keep letters GSM-7 cannot encode (e.g. Twi ɛ/ɔ) instead of
   * transliterating them. Only enable it when the user wrote in a local
   * language that needs them: one kept character makes the whole message
   * UCS-2 (70-character segments).
   */
  allowUCS2?: boolean;
}

// Typographic characters LLMs emit, mapped to GSM-7 equivalents. A single
// curly apostrophe would otherwise turn a 1-segment GSM-7 message into a
// 3-segment UCS-2 one.
const homoglyphs: Record<string, string> = {
  // Quotes and apostrophes.
  "‘": "'", "’": "'", "‚": "'", "‛": "'", "′": "'", "´": "'",
  "`": "'", "‹": "'", "›": "'",
  "“": '"', "”": '"', "„": '"', "‟": '"', "«": '"', "»": '"', "″": '"',
  // Dashes and hyphens.
  "‐": "-", "‑": "-", "‒": "-", "–": "-", "—": "-", "―": "-", "−": "-",
  "­": "",
  // Ellipsis and bullets.
  "…": "...",
  "•": "-", "·": "-", "‣": "-", "⁃": "-", "●": "-", "▪": "-", "◦": "-",
  // Maths.
  "×": "x", "÷": "/", "≤": "<=", "≥": ">=", "≠": "!=", "±": "+/-",
  "≈": "~", "√": "sqrt", "∞": "infinity", "→": "->", "←": "<-", "⇒": "=>",
  "↔": "<->", "°": " deg ", "‰": " per mille",
  "¹": "^1", "²": "^2", "³": "^3", "⁰": "^0", "⁴": "^4", "⁵": "^5",
  "⁶": "^6", "⁷": "^7", "⁸": "^8", "⁹": "^9",
  "₀": "0", "₁": "1", "₂": "2", "₃": "3", "₄": "4",
  "₅": "5", "₆": "6", "₇": "7", "₈": "8", "₉": "9",
  "½": "1/2", "¼": "1/4", "¾": "3/4", "⅓": "1/3", "⅔": "2/3",
  // Greek letters missing from GSM-7 (Δ Φ Γ Λ Ω Π Ψ Σ Θ Ξ are present).
  "α": "alpha", "β": "beta", "γ": "gamma", "δ": "delta", "ε": "epsilon",
  "θ": "theta", "λ": "lambda", "μ": "u", "µ": "u", "π": "pi",
  "ρ": "rho", "σ": "sigma", "φ": "phi", "ω": "omega",
  // Currency and marks.
  "₵": "GHS", "¢": "GHS", "₦": "NGN", "™": "TM", "©": "(c)", "®": "(R)",
  // Spaces and invisible characters.
  " ": " ", " ": " ", " ": " ", " ": " ", " ": " ", " ": " ",
  " ": " ", " ": " ", " ": " ", " ": " ", " ": " ", " ": " ", "　": " ",
  "\t": " ", " ": "\n", " ": "\n",
  "​": "", "‌": "", "‍": "", "⁠": "", "﻿": "",
};

// Ghanaian-language letters outside GSM-7, transliterated unless UCS-2 is allowed.
const localLetters: Record<string, string> = {
  "ɛ": "e", "Ɛ": "E",
  "ɔ": "o", "Ɔ": "O",
  "ŋ": "ng", "Ŋ": "Ng",
};

// Latin letters missing from GSM-7, mapped to their base letter.
// (è é ù ì ò Ç Ø ø Å å Æ æ ß É Ä Ö Ñ Ü ä ö ñ ü à are GSM-7 already.)
const accents: Record<string, string> = {
  "á": "a", "â": "a", "ã": "a", "ā": "a", "ă": "a", "ą": "a",
  "À": "A", "Á": "A", "Â": "A", "Ã": "A", "Ā": "A",
  "ç": "c", "ć": "c", "č": "c", "Ć": "C", "Č": "C",
  "ê": "e", "ë": "e", "ē": "e", "ė": "e", "ę": "e", "ě": "e",
  "È": "E", "Ê": "E", "Ë": "E", "Ē": "E",
  "í": "i", "î": "i", "ï": "i", "ī": "i", "Ì": "I", "Í": "I", "Î": "I", "Ï": "I",
  "ó": "o", "ô": "o", "õ": "o", "ō": "o", "ő": "o", "Ò": "O", "Ó": "O", "Ô": "O", "Õ": "O",
  "ú": "u", "û": "u", "ū": "u", "ű": "u", "ů": "u", "Ù": "U", "Ú": "U", "Û": "U",
  "ý": "y", "ÿ": "y", "Ý": "Y",
  "ś": "s", "š": "s", "Š": "S", "ž": "z", "Ž": "Z", "ł": "l", "Ł": "L", "ń": "n", "ř": "r",
  "œ": "oe", "Œ": "OE",
};

const has = (o: Record<string, string>, k: string) => Object.prototype.hasOwnProperty.call(o, k);

const reCedi = /gh\s?[¢₵]/gi;
const reMdLink = /\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)/g;
const reMdBold = /\*\*([^*\n]+)\*\*|__([^_\n]+)__/g;
const reMdCode = /`([^`\n]+)`/g;
const reMdHeading = /^\s{0,3}#{1,6}\s+/;
const reMdQuote = /^\s*>\s?/;
const reMdRule = /^\s*([-*_]\s*){3,}$/;
const reMdList = /^\s*(?:[-*+•▪●‣◦]|\d{1,2}[.)])\s+/;
const reFillerShort =
  /^(?:sure|certainly|of course|absolutely|great question|good question|okay|ok|alright|hello|hi there|hi)\s*[!.,:]+\s*/i;
const reFillerHere =
  /^here(?:'s| is| are)\s+(?:the\s+|an?\s+|your\s+)?(?:short\s+|brief\s+|quick\s+|concise\s+|simple\s+)?(?:answer|explanation|summary|response|reply)s?(?:\s+to\s+your\s+question)?\s*[:.!-]*\s*/i;
const reFillerLabel = /^(?:answer|response|reply)\s*:\s*/i;
const reSpaceBefore = / +([,.!?;:)])/g;
// ASCII whitespace only, matching Go's \s.
const reSpaces = /[\t\n\f\r ]+/g;
const reKeepInUCS2 = /[\p{L}\p{N}\p{P}]/u;

/** Converts raw model output to compact single-line text that stays in GSM-7 (unless allowUCS2 keeps local letters). */
export function sanitize(raw: string, opts: SanitizeOptions = {}): string {
  let s = raw.replaceAll("\r\n", "\n");
  s = s.replace(reCedi, "GHS");
  s = s.replace(reMdCode, "$1");
  s = mapChars(s, opts, true);
  s = stripMarkdown(s);
  let stripped = false;
  for (let i = 0; i < 3; i++) {
    let next = s;
    for (const re of [reFillerShort, reFillerHere, reFillerLabel]) next = next.replace(re, "");
    if (next === s) break;
    s = next;
    stripped = true;
  }
  s = mapChars(s, opts, false);
  s = s.replace(reSpaces, " ");
  s = s.replace(reSpaceBefore, "$1");
  s = s.trim();
  if (stripped) s = capitalizeFirst(s);
  return s;
}

function capitalizeFirst(s: string): string {
  const first = [...s][0];
  if (first === undefined) return s;
  const up = first.toUpperCase();
  if (first !== up && first === first.toLowerCase() && isGSM7(up)) return up + s.slice(first.length);
  return s;
}

// Applies the homoglyph table. With keepNewlines the line structure survives
// for markdown handling; the final pass also drops anything still not
// encodable (emoji, symbols, control characters).
function mapChars(s: string, opts: SanitizeOptions, keepNewlines: boolean): string {
  let out = "";
  for (const ch of s) {
    if (ch === "\n") {
      out += keepNewlines ? "\n" : " ";
    } else if (ch === "\r") {
      out += " ";
    } else if (has(homoglyphs, ch)) {
      out += homoglyphs[ch];
    } else if (isGSM7(ch)) {
      out += ch;
    } else if (has(localLetters, ch)) {
      out += opts.allowUCS2 ? ch : localLetters[ch];
    } else if (has(accents, ch)) {
      out += accents[ch];
    } else if (keepNewlines) {
      out += ch; // first pass: leave unknown characters for the final pass
    } else if (opts.allowUCS2 && reKeepInUCS2.test(ch)) {
      out += ch;
    }
    // Everything else (emoji, pictographs, combining marks, controls) is dropped.
  }
  return out;
}

function stripMarkdown(s: string): string {
  s = s.replace(reMdLink, "$1 ($2)");
  s = s.replace(reMdBold, "$1$2");
  let out = "";
  for (let line of s.split("\n")) {
    if (reMdRule.test(line)) continue;
    line = line.replace(reMdHeading, "").replace(reMdQuote, "");
    const isItem = reMdList.test(line);
    line = line.replace(reMdList, "").trim();
    if (line === "") continue;
    if (out.length > 0) {
      const last = out[out.length - 1]!;
      if (".!?:;,".includes(last)) out += " ";
      else out += isItem ? "; " : " ";
    }
    out += line;
  }
  return out;
}

/** Whether the user wrote Ghanaian letters (ɛ, ɔ, ŋ) that GSM-7 cannot carry. */
export function containsLocalLanguageLetters(s: string): boolean {
  for (const ch of s) if (has(localLetters, ch)) return true;
  return false;
}
