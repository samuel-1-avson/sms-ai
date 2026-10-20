// Phone numbers: E.164 normalisation and the Ghanaian network inferred from
// the prefix (for metrics only: ported numbers make it an approximation).

export class InvalidMsisdnError extends Error {
  constructor() {
    super("msisdn: invalid phone number");
  }
}

const GHANA_CC = "233";

/** Converts common Ghanaian and international formats to E.164: "0244123456", "233244123456", "+233 24 412 3456", "00233244123456". */
export function normalize(raw: string): string {
  raw = raw.trim();
  const international = raw.startsWith("+");
  let d = "";
  for (let i = 0; i < raw.length; i++) {
    const c = raw[i]!;
    if (c >= "0" && c <= "9") d += c;
    else if (c === "+" && i === 0) continue;
    else if (" -().".includes(c)) continue;
    else throw new InvalidMsisdnError();
  }
  if (international) {
    // already has a country code
  } else if (d.startsWith("00")) d = d.slice(2);
  else if (d.length === 10 && d[0] === "0") d = GHANA_CC + d.slice(1);
  else if (d.length === 9) d = GHANA_CC + d;

  if (d.startsWith(GHANA_CC)) {
    if (d.length !== 12) throw new InvalidMsisdnError();
  } else if (d.length < 8 || d.length > 15 || d[0] === "0") {
    throw new InvalidMsisdnError();
  }
  return "+" + d;
}

/** normalize, or null when the input is not a phone number. */
export function tryNormalize(raw: string): string | null {
  try {
    return normalize(raw);
  } catch {
    return null;
  }
}

export type Carrier = "mtn" | "telecel" | "at" | "unknown";

export function carrier(e164: string): Carrier {
  if (!e164.startsWith("+" + GHANA_CC) || e164.length !== 13) return "unknown";
  switch (e164.slice(4, 6)) {
    case "24": case "25": case "53": case "54": case "55": case "59":
      return "mtn";
    case "20": case "50":
      return "telecel";
    case "26": case "27": case "56": case "57":
      return "at";
  }
  return "unknown";
}

/** Hides all but the country code and last two digits, for logs. */
export function mask(e164: string): string {
  if (e164.length < 7) return "***";
  return e164.slice(0, 4) + "*".repeat(e164.length - 6) + e164.slice(-2);
}
