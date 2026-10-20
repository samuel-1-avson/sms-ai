import com.sun.net.httpserver.HttpServer
import expo.modules.smsgateway.brain.AiClient
import expo.modules.smsgateway.brain.AiCode
import expo.modules.smsgateway.brain.AiError
import expo.modules.smsgateway.brain.AiOptions
import expo.modules.smsgateway.brain.AnswerError
import expo.modules.smsgateway.brain.Breaker
import expo.modules.smsgateway.brain.ChatMessage
import expo.modules.smsgateway.brain.DeepInfra
import org.json.JSONObject
import java.net.InetSocketAddress
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertTrue

/** DeepInfra adapter and AiClient against a local fake of the chat completions API. */
class AiClientTest {
  private val requests = mutableListOf<JSONObject>()
  private val auth = mutableListOf<String?>()

  /** Responses by model name: status code and body. */
  private var respond: (JSONObject) -> Pair<Int, String> = { 200 to ok("Hello") }
  private val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0).apply {
    createContext("/v1/openai/chat/completions") { ex ->
      val body = JSONObject(ex.requestBody.readBytes().toString(Charsets.UTF_8))
      synchronized(requests) {
        requests += body
        auth += ex.requestHeaders.getFirst("Authorization")
      }
      val (code, out) = respond(body)
      if (code == 0) {
        Thread.sleep(3_000)
        ex.close()
        return@createContext
      }
      val bytes = out.toByteArray()
      ex.sendResponseHeaders(code, bytes.size.toLong())
      ex.responseBody.use { it.write(bytes) }
    }
    executor = java.util.concurrent.Executors.newCachedThreadPool()
    start()
  }
  private val base = "http://127.0.0.1:${server.address.port}/v1/openai/"

  @AfterTest fun stop() = server.stop(0)

  private fun ok(content: String?, reasoning: Int = 0, cost: Double? = 0.00002) = JSONObject()
    .put("model", "m")
    .put("choices", listOf(JSONObject().put("message", JSONObject().put("content", content ?: JSONObject.NULL)).put("finish_reason", "stop")))
    .put("usage", JSONObject().put("prompt_tokens", 100).put("completion_tokens", 20).apply { if (cost != null) put("estimated_cost", cost) }
      .put("completion_tokens_details", JSONObject().put("reasoning_tokens", reasoning)))
    .toString()

  private val msgs = listOf(ChatMessage("system", "s"), ChatMessage("user", "q"))

  private fun client(timeoutMs: Int = 15_000, breaker: Breaker = Breaker()) = AiClient(DeepInfra(base, "key-123", timeoutMs), AiOptions(), breaker)

  @Test fun sendsTheRightRequest() {
    val out = client().answer(msgs)
    assertEquals("Hello", out.text)
    assertEquals("openai/gpt-oss-20b", out.model)
    val req = requests.single()
    assertEquals("Bearer key-123", auth.single())
    assertEquals("openai/gpt-oss-20b", req.getString("model"))
    assertEquals("low", req.getString("reasoning_effort"))
    assertEquals(350, req.getInt("max_tokens"))
    assertEquals(false, req.getBoolean("stream"))
    assertEquals("q", req.getJSONArray("messages").getJSONObject(1).getString("content"))
    assertEquals(0.00002, out.calls.single().costUsd)
  }

  @Test fun fallsBackWhenTheReasoningModelReturnsNothing() {
    respond = { if (it.getString("model") == "openai/gpt-oss-20b") 200 to ok(null, reasoning = 350) else 200 to ok("From fallback", cost = null) }
    val out = client().answer(msgs)
    assertEquals("From fallback", out.text)
    assertEquals("meta-llama/Llama-3.3-70B-Instruct-Turbo", out.model)
    assertEquals(listOf(false, true), out.calls.map { it.ok })
    assertTrue(!requests[1].has("reasoning_effort"))
    assertEquals(300, requests[1].getInt("max_tokens"))
    assertTrue(out.calls[1].costUsd > 0, "estimated when the provider does not report it")
  }

  @Test fun rejectedKeyDoesNotTryTheFallbackAndOpensTheBreaker() {
    respond = { 401 to """{"error":"bad key"}""" }
    var t = 0L
    val breaker = Breaker(now = { t })
    val e = assertFailsWith<AnswerError> { client(breaker = breaker).answer(msgs) }
    assertEquals(AiCode.UNAUTHORIZED, (e.error as AiError).code)
    assertEquals(1, requests.size)
    val again = assertFailsWith<AnswerError> { client(breaker = breaker).answer(msgs) }
    assertEquals(AiCode.CIRCUIT_OPEN, (again.error as AiError).code)
    assertEquals(1, requests.size, "no request while open")
    t += 5 * 60_000L
    respond = { 200 to ok("back") }
    assertEquals("back", client(breaker = breaker).answer(msgs).text)
  }

  @Test fun timeoutIsRetriedOnTheFallback() {
    respond = { if (it.getString("model") == "openai/gpt-oss-20b") 0 to "" else 200 to ok("ok") }
    val out = client(timeoutMs = 500).answer(msgs)
    assertEquals("ok", out.text)
    assertTrue(out.calls[0].error!!.contains("timed out"))
  }

  @Test fun serverErrorsMapToCodes() {
    for ((status, code) in listOf(402 to AiCode.QUOTA, 429 to AiCode.RATE_LIMITED, 500 to AiCode.UPSTREAM, 400 to AiCode.BAD_REQUEST)) {
      respond = { status to "nope" }
      val e = assertFailsWith<AiError> { DeepInfra(base, "k").complete(expo.modules.smsgateway.brain.ChatRequest("x", msgs, 10, 0.2)) }
      assertEquals(code, e.code, "HTTP $status")
    }
  }

  @Test fun unreachableHostIsUpstream() {
    val e = assertFailsWith<AiError> { DeepInfra("http://127.0.0.1:1/v1/openai", "k").complete(expo.modules.smsgateway.brain.ChatRequest("x", msgs, 10, 0.2)) }
    assertEquals(AiCode.UPSTREAM, e.code)
  }

  @Test fun breakerOpensBrieflyAfterRepeatedOutages() {
    var t = 0L
    val b = Breaker(threshold = 2, now = { t })
    b.record(AiError(AiCode.UPSTREAM, "x"))
    b.allow()
    b.record(AiError(AiCode.EMPTY, "ignored"))
    b.record(AiError(AiCode.UPSTREAM, "y"))
    assertFailsWith<AiError> { b.allow() }
    assertTrue(b.state().startsWith("open"))
    t += 30_000
    b.allow()
    assertEquals("closed", b.state())
  }
}
