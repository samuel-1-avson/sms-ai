// Pure policy rules: PII scrubbing and question length bounds. Stateful
// quota enforcement lives in the repository.

// Ghana Card PIN, e.g. GHA-123456789-0.
const reGhanaCard = /\bGHA[-\s]?\d{9}[-\s]?\d\b/gi;
// 13-19 digit runs (optionally grouped by spaces/dashes): candidate card numbers.
const reCardCandidate = /\b\d(?:[ -]?\d){12,18}\b/g;
// A secret keyword followed by a value containing at least one digit.
const reSecret =
  /\b(pin|otp|password|passcode|passwd|cvv|cvc|verification code|security code)\b(\s*(?:is|was|=|:|-|no\.?|number)?\s*)([A-Za-z0-9]*\d[A-Za-z0-9]*)/gi;

/**
 * Removes Ghana Card numbers, payment card numbers and PIN/OTP/password
 * values before text is persisted or sent to the model (plan §10.2).
 * Ordinary numbers (maths, phone numbers, years) are left alone.
 */
export function scrub(s: string): { text: string; changed: boolean } {
  let out = s.replace(reGhanaCard, "[ghana card removed]");
  out = out.replace(reCardCandidate, (m) => (luhnValid(m) ? "[card number removed]" : m));
  out = out.replace(reSecret, "$1$2[removed]");
  return { text: out, changed: out !== s };
}

function luhnValid(s: string): boolean {
  const digits = [...s].filter((c) => c >= "0" && c <= "9").map(Number);
  if (digits.length < 13 || digits.length > 19) return false;
  let sum = 0;
  for (let i = digits.length - 1; i >= 0; i--) {
    let d = digits[i]!;
    if ((digits.length - 1 - i) % 2 === 1) {
      d *= 2;
      if (d > 9) d -= 9;
    }
    sum += d;
  }
  return sum % 10 === 0;
}

/** Bounds the user prompt length (in code points) sent to the model. */
export function truncateQuestion(s: string, maxChars: number): string {
  const chars = [...s.trim()];
  return chars.length <= maxChars ? chars.join("") : chars.slice(0, maxChars).join("");
}
