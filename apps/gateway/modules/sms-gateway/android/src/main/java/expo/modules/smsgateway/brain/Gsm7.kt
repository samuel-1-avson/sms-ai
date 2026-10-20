package expo.modules.smsgateway.brain

/**
 * GSM-7 / UCS-2 measurement: an exact septet and segment counter, so every
 * reply is sized the way the carrier will bill it.
 */
enum class Encoding { GSM7, UCS2 }

/** The code points of s, in order. */
fun String.cps(): IntArray {
  val out = IntArray(Character.codePointCount(this, 0, length))
  var i = 0
  var k = 0
  while (i < length) {
    val cp = Character.codePointAt(this, i)
    out[k++] = cp
    i += Character.charCount(cp)
  }
  return out
}

fun cpString(cp: Int): String = String(Character.toChars(cp))

private val basicSet: Set<Int> = GSM_BASIC.cps().toSet()
private val extSet: Set<Int> = GSM_EXTENSION.cps().toSet()

/** 1 for basic GSM-7 characters, 2 for extension characters, 0 when GSM-7 cannot represent it. */
fun septetCost(cp: Int): Int = when (cp) {
  in basicSet -> 1
  in extSet -> 2
  else -> 0
}

fun isGsm7(cp: Int): Boolean = septetCost(cp) > 0

fun isGsm7Text(s: String): Boolean = s.cps().all { isGsm7(it) }

/** Cost of one code point in the given encoding's units. */
fun unitCost(enc: Encoding, cp: Int): Int = if (enc == Encoding.GSM7) septetCost(cp) else if (cp > 0xFFFF) 2 else 1

/** The encoding a carrier would use for s and its length in that encoding's units. */
fun measure(s: String): Pair<Encoding, Int> {
  val enc = if (isGsm7Text(s)) Encoding.GSM7 else Encoding.UCS2
  return enc to s.cps().sumOf { unitCost(enc, it) }
}

const val GSM7_SINGLE = 160
const val GSM7_MULTI = 153
const val UCS2_SINGLE = 70
const val UCS2_MULTI = 67

/** How many SMS parts s will be billed as. An escape or surrogate pair is never split across parts. */
fun segmentCount(s: String): Int {
  val (enc, n) = measure(s)
  if (n == 0) return 0
  val single = if (enc == Encoding.UCS2) UCS2_SINGLE else GSM7_SINGLE
  val multi = if (enc == Encoding.UCS2) UCS2_MULTI else GSM7_MULTI
  if (n <= single) return 1
  var segs = 1
  var used = 0
  for (cp in s.cps()) {
    val c = unitCost(enc, cp)
    if (used + c > multi) {
      segs++
      used = 0
    }
    used += c
  }
  return segs
}
