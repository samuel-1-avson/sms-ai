package expo.modules.smsgateway.brain

/** Phone numbers: E.164 normalisation, and which senders are people worth answering. */

private const val GHANA_CC = "233"

/**
 * Converts common Ghanaian and international formats to E.164 ("0244123456",
 * "233244123456", "+233 24 412 3456", "00233244123456"), or returns null when
 * the input is not a phone number (a network name like "MTN", a short code).
 */
fun normalizeNumber(input: String): String? {
  val raw = jsTrim(input)
  val international = raw.startsWith("+")
  val d = StringBuilder()
  for ((i, c) in raw.withIndex()) {
    when {
      c in '0'..'9' -> d.append(c)
      c == '+' && i == 0 -> {}
      " -().".indexOf(c) >= 0 -> {}
      else -> return null
    }
  }
  var n = d.toString()
  if (international) {
    // already has a country code
  } else if (n.startsWith("00")) n = n.substring(2)
  else if (n.length == 10 && n[0] == '0') n = GHANA_CC + n.substring(1)
  else if (n.length == 9) n = GHANA_CC + n

  if (n.startsWith(GHANA_CC)) {
    if (n.length != 12) return null
  } else if (n.length < 8 || n.length > 15 || n.isEmpty() || n[0] == '0') {
    return null
  }
  return "+$n"
}

/**
 * The number to answer, or null for senders that must never get a reply:
 * network names and promos ("MTN", "Telecel"), short codes, and this phone's
 * own number (which would make it answer itself in a loop).
 */
fun replyableSender(sender: String, ownNumber: String?): String? {
  val n = normalizeNumber(sender) ?: return null
  if (!Regex("""\+[0-9]{8,15}""").matches(n)) return null
  if (ownNumber != null && normalizeNumber(ownNumber) == n) return null
  return n
}

/** Hides all but the country code and last two digits, for the activity log. */
fun maskNumber(e164: String): String {
  if (e164.length < 7) return "***"
  return e164.substring(0, 4) + "*".repeat(e164.length - 6) + e164.substring(e164.length - 2)
}
