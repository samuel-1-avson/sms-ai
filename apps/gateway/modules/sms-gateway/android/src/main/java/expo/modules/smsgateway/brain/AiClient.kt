package expo.modules.smsgateway.brain

import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.SocketTimeoutException
import java.net.URL

/**
 * The AI side: DeepInfra's OpenAI-compatible chat completions API, with the
 * reasoning-token guard, one fallback model attempt and a circuit breaker.
 */

data class ChatRequest(
  val model: String,
  val messages: List<ChatMessage>,
  val maxOutputTokens: Int,
  val temperature: Double,
  /** "low" for reasoning models, null otherwise. */
  val reasoningEffort: String? = null,
)

data class ChatResult(
  val content: String = "",
  val model: String = "",
  val finishReason: String = "",
  val inputTokens: Int = 0,
  val outputTokens: Int = 0,
  val reasoningTokens: Int = 0,
  /** Provider-reported cost when available. */
  val costUsd: Double = 0.0,
  val latencyMs: Long = 0,
)

enum class AiCode {
  /** No visible text: a reasoning model spent its whole budget thinking. */
  EMPTY,
  TIMEOUT,
  RATE_LIMITED,

  /** Out of credit. */
  QUOTA,

  /** The API key was rejected. */
  UNAUTHORIZED,
  UPSTREAM,
  BAD_REQUEST,
  CIRCUIT_OPEN,
}

/** A model failure. [result] carries the tokens spent so they are still counted. */
class AiError(val code: AiCode, message: String, val result: ChatResult = ChatResult()) : Exception("ai: $message")

/** Whether a later attempt may succeed. */
fun isRetryable(e: Throwable?): Boolean =
  e is AiError && e.code in setOf(AiCode.TIMEOUT, AiCode.RATE_LIMITED, AiCode.UPSTREAM, AiCode.EMPTY, AiCode.CIRCUIT_OPEN)

/** Models that emit hidden chain-of-thought tokens and need reasoning_effort plus a larger budget. */
fun isReasoningModel(model: String): Boolean {
  val m = model.lowercase()
  return listOf("gpt-oss", "deepseek-r1", "qwq", "-thinking", "o1-", "o3-", "o4-").any { m.contains(it) }
}

/** One chat completion. Implementations throw [AiError] on failure. */
fun interface ChatModel {
  fun complete(req: ChatRequest): ChatResult
}

const val DEEPINFRA_BASE_URL = "https://api.deepinfra.com/v1/openai"

class DeepInfra(
  baseUrl: String,
  private val apiKey: String,
  private val timeoutMs: Int = 15_000,
) : ChatModel {
  private val url = baseUrl.trimEnd('/') + "/chat/completions"

  override fun complete(req: ChatRequest): ChatResult {
    val body = JSONObject()
      .put("model", req.model)
      .put("messages", JSONArray(req.messages.map { JSONObject().put("role", it.role).put("content", it.content) }))
      .put("temperature", req.temperature)
      .put("stream", false)
    if (req.maxOutputTokens > 0) body.put("max_tokens", req.maxOutputTokens)
    if (!req.reasoningEffort.isNullOrEmpty()) body.put("reasoning_effort", req.reasoningEffort)

    val start = System.nanoTime()
    fun elapsed() = (System.nanoTime() - start) / 1_000_000
    val status: Int
    val raw: String
    try {
      val conn = URL(url).openConnection() as HttpURLConnection
      try {
        conn.requestMethod = "POST"
        conn.connectTimeout = 10_000
        conn.readTimeout = timeoutMs
        conn.doOutput = true
        conn.setRequestProperty("Authorization", "Bearer $apiKey")
        conn.setRequestProperty("Content-Type", "application/json")
        conn.outputStream.use { it.write(body.toString().toByteArray(Charsets.UTF_8)) }
        status = conn.responseCode
        val stream = if (status in 200..299) conn.inputStream else conn.errorStream
        raw = stream?.use { it.readBytes().toString(Charsets.UTF_8) } ?: ""
      } finally {
        conn.disconnect()
      }
    } catch (e: SocketTimeoutException) {
      throw AiError(AiCode.TIMEOUT, "request timed out after ${elapsed()} ms", ChatResult(latencyMs = elapsed()))
    } catch (e: IOException) {
      throw AiError(AiCode.UPSTREAM, "${e.javaClass.simpleName}: ${e.message ?: "network error"}", ChatResult(latencyMs = elapsed()))
    }
    val latencyMs = elapsed()
    if (status != 200) throw statusError(status, raw, latencyMs)

    val parsed = try {
      JSONObject(raw)
    } catch (e: Exception) {
      throw AiError(AiCode.UPSTREAM, "decoding response: ${e.message}", ChatResult(latencyMs = latencyMs))
    }
    val choice = parsed.optJSONArray("choices")?.optJSONObject(0)
    val message = choice?.optJSONObject("message")
    val usage = parsed.optJSONObject("usage")
    val res = ChatResult(
      content = (if (message == null || message.isNull("content")) "" else message.optString("content")).trim(),
      model = parsed.optString("model").ifEmpty { req.model },
      finishReason = choice?.optString("finish_reason") ?: "",
      inputTokens = usage?.optInt("prompt_tokens") ?: 0,
      outputTokens = usage?.optInt("completion_tokens") ?: 0,
      reasoningTokens = usage?.optJSONObject("completion_tokens_details")?.optInt("reasoning_tokens") ?: 0,
      costUsd = usage?.optDouble("estimated_cost", 0.0)?.takeIf { !it.isNaN() } ?: 0.0,
      latencyMs = latencyMs,
    )
    if (res.content.isEmpty()) {
      // The reasoning-token trap: tokens were spent but nothing visible came back.
      throw AiError(AiCode.EMPTY, "empty response (finish_reason=\"${res.finishReason}\", reasoning_tokens=${res.reasoningTokens})", res)
    }
    return res
  }

  private fun statusError(code: Int, body: String, latencyMs: Long): AiError {
    val snippet = body.trim().take(200)
    val r = ChatResult(latencyMs = latencyMs)
    return when {
      code == 401 || code == 403 -> AiError(AiCode.UNAUTHORIZED, "HTTP $code: the API key was rejected", r)
      code == 402 -> AiError(AiCode.QUOTA, "HTTP $code: out of credit", r)
      code == 429 -> AiError(AiCode.RATE_LIMITED, "HTTP $code", r)
      code == 408 || code == 504 -> AiError(AiCode.TIMEOUT, "HTTP $code", r)
      code >= 500 -> AiError(AiCode.UPSTREAM, "HTTP $code: $snippet", r)
      else -> AiError(AiCode.BAD_REQUEST, "HTTP $code: $snippet", r)
    }
  }
}

/** Leaves room for ~150 reasoning tokens plus a short visible answer. */
const val MIN_REASONING_MAX_TOKENS = 350

/** One model attempt, for the usage counters. */
data class CallRecord(val model: String, val ok: Boolean, val costUsd: Double, val latencyMs: Long, val error: String? = null)

data class Outcome(val text: String, val model: String, val calls: List<CallRecord>)

/** Thrown by [AiClient.answer]; carries the attempts made so their cost is still counted. */
class AnswerError(val error: Throwable, val calls: List<CallRecord>) : Exception(error.message, error)

data class AiOptions(
  val primary: String = "openai/gpt-oss-20b",
  val fallback: String? = "meta-llama/Llama-3.3-70B-Instruct-Turbo",
  val reasoningEffort: String = "low",
  val maxOutputTokens: Int = 300,
  val temperature: Double = 0.2,
)

class AiClient(private val model: ChatModel, private val opts: AiOptions, private val breaker: Breaker) {
  /** Asks the primary model, then the fallback once if the primary returned empty text or a retryable error. */
  fun answer(messages: List<ChatMessage>): Outcome {
    val calls = mutableListOf<CallRecord>()
    try {
      breaker.allow()
    } catch (e: AiError) {
      throw AnswerError(e, calls)
    }
    val models = listOfNotNull(opts.primary, opts.fallback?.takeIf { it.isNotBlank() && it != opts.primary })
    var lastErr: Throwable? = null
    for ((i, m) in models.withIndex()) {
      if (i > 0 && !isRetryable(lastErr)) break
      var res: ChatResult? = null
      var err: Throwable? = null
      try {
        res = model.complete(request(m, messages))
      } catch (e: AiError) {
        err = e
        res = e.result
      } catch (e: Exception) {
        err = AiError(AiCode.UPSTREAM, "${e.javaClass.simpleName}: ${e.message}")
      }
      calls += CallRecord(
        model = m,
        ok = err == null,
        costUsd = res?.costUsd?.takeIf { it > 0 } ?: estimateCost(m, res?.inputTokens ?: 0, res?.outputTokens ?: 0),
        latencyMs = res?.latencyMs ?: 0,
        error = err?.message,
      )
      breaker.record(err)
      if (err == null && res != null) return Outcome(res.content, m, calls)
      lastErr = err
    }
    throw AnswerError(lastErr ?: AiError(AiCode.TIMEOUT, "no attempt was made"), calls)
  }

  private fun request(m: String, messages: List<ChatMessage>): ChatRequest =
    if (isReasoningModel(m)) {
      ChatRequest(m, messages, maxOf(opts.maxOutputTokens, MIN_REASONING_MAX_TOKENS), opts.temperature, opts.reasoningEffort.ifEmpty { "low" })
    } else {
      ChatRequest(m, messages, opts.maxOutputTokens, opts.temperature)
    }
}

// Planning prices in USD per 1M tokens (input, output). Unknown models use a
// deliberately high price so the daily spend cap errs on the safe side.
private val prices = mapOf("openai/gpt-oss-20b" to (0.03 to 0.14))
private val unknownPrice = 0.6 to 0.6

fun estimateCost(model: String, input: Int, output: Int): Double {
  val (inP, outP) = prices[model] ?: unknownPrice
  return input / 1e6 * inP + output / 1e6 * outP
}

/**
 * A small circuit breaker. A rejected key or exhausted credit opens it for a
 * long cooldown; bursts of transient failures open it briefly. Shared across
 * calls so the phone stops hammering a provider that is down.
 */
class Breaker(
  private val threshold: Int = 5,
  private val shortCooldownMs: Long = 30_000,
  private val longCooldownMs: Long = 5 * 60_000,
  private val now: () -> Long = System::currentTimeMillis,
) {
  private var failures = 0
  private var openUntil = 0L
  private var reason = ""

  @Synchronized fun allow() {
    if (now() < openUntil) throw AiError(AiCode.CIRCUIT_OPEN, "paused after errors: $reason")
  }

  @Synchronized fun record(err: Throwable?) {
    when {
      err == null -> failures = 0
      err is AiError && (err.code == AiCode.QUOTA || err.code == AiCode.UNAUTHORIZED) -> open(longCooldownMs, err)
      // A request-specific problem or an empty reasoning answer (handled by the fallback): not an outage.
      err is AiError && (err.code == AiCode.BAD_REQUEST || err.code == AiCode.EMPTY) -> {}
      ++failures >= threshold -> open(shortCooldownMs, err)
    }
  }

  private fun open(ms: Long, err: Throwable) {
    openUntil = now() + ms
    reason = (err.message ?: err.toString()).removePrefix("ai: ")
    failures = 0
  }

  /** "closed" or "open: <reason>". */
  @Synchronized fun state(): String = if (now() < openUntil) "open: $reason" else "closed"
}
