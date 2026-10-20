import expo.modules.smsgateway.brain.Command
import expo.modules.smsgateway.brain.Encoding
import expo.modules.smsgateway.brain.Turn
import expo.modules.smsgateway.brain.buildPrompt
import expo.modules.smsgateway.brain.containsLocalLanguageLetters
import expo.modules.smsgateway.brain.estimateTokens
import expo.modules.smsgateway.brain.measure
import expo.modules.smsgateway.brain.normalizeNumber
import expo.modules.smsgateway.brain.paginate
import expo.modules.smsgateway.brain.parseCommand
import expo.modules.smsgateway.brain.sanitize
import expo.modules.smsgateway.brain.scrub
import expo.modules.smsgateway.brain.segmentCount
import expo.modules.smsgateway.brain.splitFirst
import expo.modules.smsgateway.brain.truncate
import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant
import kotlin.test.Test
import kotlin.test.assertEquals

/**
 * Parity with the TypeScript implementation (tag server-relay-v1): the
 * fixtures were produced by running the tested TS code over these inputs.
 */
class FixturesTest {
  private val fx = JSONObject(javaClass.getResource("/fixtures.json")!!.readText())

  private fun each(name: String, f: (JSONObject) -> Unit) {
    val arr = fx.getJSONArray(name)
    val failures = mutableListOf<String>()
    for (i in 0 until arr.length()) {
      val c = arr.getJSONObject(i)
      try {
        f(c)
      } catch (e: AssertionError) {
        failures += "$name[$i] ${JSONObject.quote(c.optString("input"))}: ${e.message}"
      }
    }
    if (failures.isNotEmpty()) throw AssertionError(failures.joinToString("\n"))
  }

  private fun JSONArray.strings() = (0 until length()).map { getString(it) }

  @Test fun measure() = each("measure") { c ->
    val (enc, n) = measure(c.getString("input"))
    assertEquals(c.getString("encoding"), if (enc == Encoding.GSM7) "gsm7" else "ucs2")
    assertEquals(c.getInt("units"), n)
  }

  @Test fun segments() = each("segments") { c -> assertEquals(c.getInt("segments"), segmentCount(c.getString("input"))) }

  @Test fun sanitize() = each("sanitize") { c ->
    val input = c.getString("input")
    assertEquals(c.getString("gsm7"), sanitize(input), "gsm7")
    assertEquals(c.getString("ucs2"), sanitize(input, allowUcs2 = true), "ucs2")
    assertEquals(c.getBoolean("local"), containsLocalLanguageLetters(input), "local")
  }

  @Test fun split() = each("split") { c ->
    val input = c.getString("input")
    assertEquals(c.getString("first") to c.getString("rest"), splitFirst(input), "splitFirst")
    assertEquals(c.getString("truncated"), truncate(input), "truncate")
    assertEquals(c.getJSONArray("pages3").strings(), paginate(input, 3), "paginate")
  }

  @Test fun commands() = each("commands") { c ->
    val want = if (c.isNull("kind")) null else Command.valueOf(c.getString("kind"))
    assertEquals(want, parseCommand(c.getString("input")))
  }

  @Test fun scrub() = each("scrub") { c -> assertEquals(c.getString("output"), scrub(c.getString("input"))) }

  @Test fun senders() = each("senders") { c ->
    assertEquals(if (c.isNull("normalized")) null else c.getString("normalized"), normalizeNumber(c.getString("input")))
  }

  @Test fun tokens() = each("tokens") { c -> assertEquals(c.getInt("tokens"), estimateTokens(c.getString("input"))) }

  @Test fun prompt() = each("prompt") { c ->
    val h = c.getJSONArray("history")
    val history = (0 until h.length()).map { Turn(h.getJSONObject(it).getString("role"), h.getJSONObject(it).getString("content")) }
    val now = if (c.isNull("now")) null else Instant.parse(c.getString("now")).toEpochMilli()
    val got = buildPrompt(c.getString("question"), history, c.getInt("maxInputTokens"), now)
    val m = c.getJSONArray("messages")
    val want = (0 until m.length()).map { m.getJSONObject(it).getString("role") to m.getJSONObject(it).getString("content") }
    assertEquals(want, got.map { it.role to it.content })
  }
}
