// GSM-7 / UCS-2 measurement: an exact septet and segment counter (plan
// §6.2, ADR-005). Nothing here calls the LLM.

export type Encoding = "gsm7" | "ucs2";

// GSM 03.38 default alphabet (the ESC code point is deliberately omitted).
const GSM_BASIC =
  "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
  "¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà";

// GSM 03.38 extension table: each costs two septets (ESC + char).
const GSM_EXTENSION = "^{}\\[~]|€";

const basicSet = new Set(GSM_BASIC);
const extSet = new Set(GSM_EXTENSION);

/** 1 for basic GSM-7 characters, 2 for extension characters, 0 when GSM-7 cannot represent it. */
export function septetCost(ch: string): number {
  if (basicSet.has(ch)) return 1;
  if (extSet.has(ch)) return 2;
  return 0;
}

export function isGSM7(ch: string): boolean {
  return septetCost(ch) > 0;
}

/** Whether every character of s is GSM-7 encodable. */
export function isGSM7Text(s: string): boolean {
  for (const ch of s) if (!isGSM7(ch)) return false;
  return true;
}

/** Cost of one code point in the given encoding's units. */
export function unitCost(enc: Encoding, ch: string): number {
  if (enc === "gsm7") return septetCost(ch);
  return ch.codePointAt(0)! > 0xffff ? 2 : 1;
}

/** The encoding a carrier would use for s and its length in that encoding's units. */
export function measure(s: string): [Encoding, number] {
  const enc: Encoding = isGSM7Text(s) ? "gsm7" : "ucs2";
  let n = 0;
  for (const ch of s) n += unitCost(enc, ch);
  return [enc, n];
}

// Segment limits: single-part and per-part sizes once a User Data Header is
// needed for concatenation.
export const GSM7_SINGLE = 160;
export const GSM7_MULTI = 153;
export const UCS2_SINGLE = 70;
export const UCS2_MULTI = 67;

/**
 * How many SMS parts s will be billed as. An escape pair or surrogate pair
 * is never split across parts, matching handset and SMSC behaviour.
 */
export function segmentCount(s: string): number {
  const [enc, n] = measure(s);
  if (n === 0) return 0;
  const single = enc === "ucs2" ? UCS2_SINGLE : GSM7_SINGLE;
  const multi = enc === "ucs2" ? UCS2_MULTI : GSM7_MULTI;
  if (n <= single) return 1;
  let segs = 1;
  let used = 0;
  for (const ch of s) {
    const c = unitCost(enc, ch);
    if (used + c > multi) {
      segs++;
      used = 0;
    }
    used += c;
  }
  return segs;
}

/** A message as the carrier will bill it. */
export interface Stats {
  encoding: Encoding;
  units: number;
  segments: number;
}

export function analyze(s: string): Stats {
  const [encoding, units] = measure(s);
  return { encoding, units, segments: segmentCount(s) };
}
