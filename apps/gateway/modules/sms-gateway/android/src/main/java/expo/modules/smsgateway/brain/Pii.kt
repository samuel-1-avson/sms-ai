package expo.modules.smsgateway.brain

/**
 * PII scrubbing: Ghana Card numbers, payment card numbers and PIN/OTP/password
 * values are removed before a question is stored or sent to the model.
 * Ordinary numbers (maths, phone numbers, years) are left alone.
 */

// \b, \d and \s spelled out (see Sanitize.kt): ASCII word boundaries, as in JavaScript.
private const val B_BEFORE = "(?<![A-Za-z0-9_])"
private const val B_AFTER = "(?![A-Za-z0-9_])"

// Ghana Card PIN, e.g. GHA-123456789-0.
private val reGhanaCard = Regex("""(?i)${B_BEFORE}GHA(?:-|$SP)?[0-9]{9}(?:-|$SP)?[0-9]$B_AFTER""")
// 13-19 digit runs (optionally grouped by spaces/dashes): candidate card numbers.
private val reCardCandidate = Regex("""$B_BEFORE[0-9](?:[ -]?[0-9]){12,18}$B_AFTER""")
// A secret keyword followed by a value containing at least one digit.
private val reSecret = Regex(
  """(?i)$B_BEFORE(pin|otp|password|passcode|passwd|cvv|cvc|verification code|security code)$B_AFTER""" +
    """($SP*(?:is|was|=|:|-|no\.?|number)?$SP*)([A-Za-z0-9]*[0-9][A-Za-z0-9]*)""",
)

fun scrub(s: String): String {
  var out = reGhanaCard.replace(s, "[ghana card removed]")
  out = reCardCandidate.replace(out) { if (luhnValid(it.value)) "[card number removed]" else it.value }
  out = reSecret.replace(out) { it.groupValues[1] + it.groupValues[2] + "[removed]" }
  return out
}

private fun luhnValid(s: String): Boolean {
  val digits = s.filter { it in '0'..'9' }.map { it - '0' }
  if (digits.size < 13 || digits.size > 19) return false
  var sum = 0
  for (i in digits.indices.reversed()) {
    var d = digits[i]
    if ((digits.size - 1 - i) % 2 == 1) {
      d *= 2
      if (d > 9) d -= 9
    }
    sum += d
  }
  return sum % 10 == 0
}

/** Bounds a question's length in code points. */
fun truncateQuestion(s: String, maxChars: Int): String {
  val cps = jsTrim(s).cps()
  return if (cps.size <= maxChars) String(cps, 0, cps.size) else String(cps, 0, maxChars)
}
