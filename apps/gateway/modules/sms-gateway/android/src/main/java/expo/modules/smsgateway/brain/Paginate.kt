package expo.modules.smsgateway.brain

/** The MORE pager: sentence-aware splitting of an answer into single-part SMS pages. */

/** Appended to a page that has a continuation. */
const val SMS_MORE_SUFFIX = " ..Txt MORE"

private fun limitFor(enc: Encoding) = if (enc == Encoding.UCS2) UCS2_SINGLE else GSM7_SINGLE

private enum class Cut { ALL, SENTENCE, WORD, HARD }

// Finds the index (in UTF-16 units) at which to cut text so the head fits in
// budget units, preferring the end of a sentence, then a word boundary, then a
// hard cut.
private fun cutIndex(text: String, enc: Encoding, budget: Int): Pair<Int, Cut> {
  if (budget <= 0) return 0 to Cut.HARD
  var used = 0
  var lastFit = 0
  var lastSpace = -1
  var spaceUnits = 0
  var lastSentence = -1
  var sentenceUnits = 0
  var prevTerminal = false
  var i = 0
  for (cp in text.cps()) {
    val c = unitCost(enc, cp)
    if (used + c > budget) break
    if (cp == ' '.code) {
      lastSpace = i
      spaceUnits = used
      if (prevTerminal) {
        lastSentence = i
        sentenceUnits = used
      }
    }
    used += c
    lastFit = i + Character.charCount(cp)
    prevTerminal = cp == '.'.code || cp == '!'.code || cp == '?'.code
    i += Character.charCount(cp)
  }
  if (lastFit >= text.length) return text.length to Cut.ALL
  if (text[lastFit] == ' ') {
    lastSpace = lastFit
    spaceUnits = used
    if (prevTerminal) {
      lastSentence = lastFit
      sentenceUnits = used
    }
  }
  if (lastSentence > 0 && sentenceUnits * 2 >= budget) return lastSentence to Cut.SENTENCE
  if (lastSpace > 0 && spaceUnits * 10 >= budget * 3) return lastSpace to Cut.WORD
  return lastFit to Cut.HARD
}

private fun unitsOf(enc: Encoding, s: String) = s.cps().sumOf { unitCost(enc, it) }

private fun trimRightChars(s: String, chars: String): String {
  var end = s.length
  while (end > 0 && chars.indexOf(s[end - 1]) >= 0) end--
  return s.substring(0, end)
}

/** Whether text fits on one page. */
fun fits(text: String): Boolean {
  val (enc, n) = measure(text)
  return n <= limitFor(enc)
}

/** The first page (with the suffix when there is a continuation) and the remaining text. The first page always fits. */
fun splitFirst(input: String, suffix: String = SMS_MORE_SUFFIX): Pair<String, String> {
  val text = jsTrim(input)
  val (enc, n) = measure(text)
  val limit = limitFor(enc)
  if (n <= limit) return text to ""
  val budget = limit - unitsOf(enc, suffix)
  if (budget < limit / 3.0) return truncate(text) to ""
  val (idx, kind) = cutIndex(text, enc, budget)
  var head = jsTrim(text.substring(0, idx))
  // Mid-sentence: drop dangling punctuation so the page reads cleanly.
  if (kind == Cut.WORD) head = trimRightChars(head, " ,;:-")
  if (head.isEmpty()) return truncate(text) to ""
  return (head + suffix) to jsTrim(text.substring(idx))
}

/** Shortens text to one page, ending on a full sentence when one is long enough, otherwise on a word plus "...". */
fun truncate(input: String): String {
  val text = jsTrim(input)
  val (enc, n) = measure(text)
  val limit = limitFor(enc)
  if (n <= limit) return text
  val (sIdx, kind) = cutIndex(text, enc, limit)
  if (kind == Cut.SENTENCE) return jsTrim(text.substring(0, sIdx))
  val (idx, _) = cutIndex(text, enc, limit - 3)
  return trimRightChars(text.substring(0, idx), " ,;:-") + "..."
}

/** Splits text into at most maxPages pages; every page but the last ends with suffix. */
fun paginate(text: String, maxPages: Int, suffix: String = SMS_MORE_SUFFIX): List<String> {
  val max = maxPages.coerceAtLeast(1)
  val pages = mutableListOf<String>()
  var rest = jsTrim(text)
  while (pages.size < max - 1) {
    val (first, r) = splitFirst(rest, suffix)
    pages.add(first)
    if (r.isEmpty()) return pages
    rest = r
  }
  pages.add(truncate(rest))
  return pages
}
