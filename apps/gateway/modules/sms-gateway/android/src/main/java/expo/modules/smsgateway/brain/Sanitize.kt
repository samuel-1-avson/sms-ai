package expo.modules.smsgateway.brain

/**
 * Deterministic GSM-7 sanitizer: turns raw model output into compact
 * single-line text that stays in GSM-7 ("the homoglyph trap": one curly
 * apostrophe would turn a 1-part GSM-7 reply into a 3-part UCS-2 one).
 *
 * The patterns spell out their character classes instead of using \s, \d or
 * \b, because Android's regex engine (ICU) treats those as Unicode-aware while
 * the JVM does not; explicit classes behave the same on both.
 */

// JavaScript's \s, which the original TypeScript patterns used.
internal const val SP = """[\t\n\u000B\f\r    -     　﻿]"""

private val reCedi = Regex("""(?i)gh$SP?[¢₵]""")
private val reMdLink = Regex("""\[([^\]\n]+)\]\((https?://[^)\t\n\u000B\f\r    -     　﻿]+)\)""")
private val reMdBold = Regex("""\*\*([^*\n]+)\*\*|__([^_\n]+)__""")
private val reMdCode = Regex("""`([^`\n]+)`""")
private val reMdHeading = Regex("""^$SP{0,3}#{1,6}$SP+""")
private val reMdQuote = Regex("""^$SP*>$SP?""")
private val reMdRule = Regex("""$SP*([-*_]$SP*){3,}""")
private val reMdList = Regex("""^$SP*(?:[-*+•▪●‣◦]|[0-9]{1,2}[.)])$SP+""")
private val reFillerShort =
  Regex("""(?i)^(?:sure|certainly|of course|absolutely|great question|good question|okay|ok|alright|hello|hi there|hi)$SP*[!.,:]+$SP*""")
private val reFillerHere = Regex(
  """(?i)^here(?:'s| is| are)$SP+(?:the$SP+|an?$SP+|your$SP+)?(?:short$SP+|brief$SP+|quick$SP+|concise$SP+|simple$SP+)?""" +
    """(?:answer|explanation|summary|response|reply)s?(?:$SP+to$SP+your$SP+question)?$SP*[:.!-]*$SP*""",
)
private val reFillerLabel = Regex("""(?i)^(?:answer|response|reply)$SP*:$SP*""")
private val reSpaceBefore = Regex(""" +([,.!?;:)])""")
// ASCII whitespace only.
private val reSpaces = Regex("""[\t\n\f\r ]+""")

/**
 * Converts raw model output to compact single-line text that stays in GSM-7.
 * With [allowUcs2], Ghanaian letters such as ɛ and ɔ are kept (which makes the
 * whole message UCS-2, 70 characters per part); use it only when the user
 * wrote them.
 */
fun sanitize(raw: String, allowUcs2: Boolean = false): String {
  var s = raw.replace("\r\n", "\n")
  s = reCedi.replace(s, "GHS")
  s = reMdCode.replace(s) { it.groupValues[1] }
  s = mapChars(s, allowUcs2, keepNewlines = true)
  s = stripMarkdown(s)
  var stripped = false
  for (i in 0 until 3) {
    var next = s
    for (re in listOf(reFillerShort, reFillerHere, reFillerLabel)) next = re.replaceFirst(next, "")
    if (next == s) break
    s = next
    stripped = true
  }
  s = mapChars(s, allowUcs2, keepNewlines = false)
  s = reSpaces.replace(s, " ")
  s = reSpaceBefore.replace(s) { it.groupValues[1] }
  s = jsTrim(s)
  if (stripped) s = capitalizeFirst(s)
  return s
}

private fun capitalizeFirst(s: String): String {
  if (s.isEmpty()) return s
  val first = Character.codePointAt(s, 0)
  val f = cpString(first)
  val up = f.uppercase()
  val upCps = up.cps()
  if (f != up && f == f.lowercase() && upCps.size == 1 && isGsm7(upCps[0])) return up + s.substring(f.length)
  return s
}

private fun keepInUcs2(cp: Int): Boolean = when (Character.getType(cp)) {
  Character.UPPERCASE_LETTER.toInt(), Character.LOWERCASE_LETTER.toInt(), Character.TITLECASE_LETTER.toInt(),
  Character.MODIFIER_LETTER.toInt(), Character.OTHER_LETTER.toInt(),
  Character.DECIMAL_DIGIT_NUMBER.toInt(), Character.LETTER_NUMBER.toInt(), Character.OTHER_NUMBER.toInt(),
  Character.CONNECTOR_PUNCTUATION.toInt(), Character.DASH_PUNCTUATION.toInt(), Character.START_PUNCTUATION.toInt(),
  Character.END_PUNCTUATION.toInt(), Character.INITIAL_QUOTE_PUNCTUATION.toInt(), Character.FINAL_QUOTE_PUNCTUATION.toInt(),
  Character.OTHER_PUNCTUATION.toInt(),
  -> true
  else -> false
}

// Applies the character tables. With keepNewlines the line structure survives
// for markdown handling; the final pass also drops anything still not
// encodable (emoji, symbols, control characters).
private fun mapChars(s: String, allowUcs2: Boolean, keepNewlines: Boolean): String {
  val out = StringBuilder(s.length)
  for (cp in s.cps()) {
    val homoglyph = HOMOGLYPHS[cp]
    val local = LOCAL_LETTERS[cp]
    val accent = ACCENTS[cp]
    when {
      cp == '\n'.code -> out.append(if (keepNewlines) "\n" else " ")
      cp == '\r'.code -> out.append(' ')
      homoglyph != null -> out.append(homoglyph)
      isGsm7(cp) -> out.appendCodePoint(cp)
      local != null -> if (allowUcs2) out.appendCodePoint(cp) else out.append(local)
      accent != null -> out.append(accent)
      keepNewlines -> out.appendCodePoint(cp) // first pass: leave unknown characters for the final pass
      allowUcs2 && keepInUcs2(cp) -> out.appendCodePoint(cp)
      // Everything else (emoji, pictographs, combining marks, controls) is dropped.
    }
  }
  return out.toString()
}

private fun stripMarkdown(input: String): String {
  var s = reMdLink.replace(input) { "${it.groupValues[1]} (${it.groupValues[2]})" }
  s = reMdBold.replace(s) { it.groupValues[1] + it.groupValues[2] }
  val out = StringBuilder()
  for (raw in s.split("\n")) {
    if (reMdRule.matches(raw)) continue
    var line = reMdQuote.replaceFirst(reMdHeading.replaceFirst(raw, ""), "")
    val isItem = reMdList.containsMatchIn(line)
    line = jsTrim(reMdList.replaceFirst(line, ""))
    if (line.isEmpty()) continue
    if (out.isNotEmpty()) {
      val last = out[out.length - 1]
      out.append(if (".!?:;,".indexOf(last) >= 0) " " else if (isItem) "; " else " ")
    }
    out.append(line)
  }
  return out.toString()
}

/** String.prototype.trim: strips JavaScript whitespace and line terminators. */
internal fun jsTrim(s: String): String = s.trim { isJsSpace(it) }

internal fun isJsSpace(c: Char): Boolean = when (c) {
  '\t', '\n', '\u000B', '\u000C', '\r', ' ', ' ', ' ', ' ', ' ', ' ', ' ', '　', '﻿' -> true
  else -> c in ' '..' '
}

/** Whether the user wrote Ghanaian letters (ɛ, ɔ, ŋ) that GSM-7 cannot carry. */
fun containsLocalLanguageLetters(s: String): Boolean = s.cps().any { it in LOCAL_LETTERS }
