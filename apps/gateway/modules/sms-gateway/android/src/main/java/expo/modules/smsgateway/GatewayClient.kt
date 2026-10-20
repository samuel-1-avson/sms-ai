package expo.modules.smsgateway

import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec
import org.json.JSONObject

/** Thrown for any answer other than 2xx; 401 means the pairing (device secret) is wrong. */
class GatewayHttpException(val status: Int, message: String) : IOException(message)

/**
 * The phone side of the /gateway/v1 protocol (packages/shared/src/protocol.ts).
 * Every request is a POST whose raw body is signed exactly like the server
 * expects (packages/shared/src/signing.ts).
 */
class GatewayClient(private val baseUrl: String, private val secret: String) {

  fun post(path: String, body: JSONObject, readTimeoutMs: Int = 20_000): JSONObject {
    val raw = body.toString()
    val ts = System.currentTimeMillis() / 1000
    val conn = (URL(baseUrl.trimEnd('/') + path).openConnection() as HttpURLConnection).apply {
      requestMethod = "POST"
      connectTimeout = 15_000
      readTimeout = readTimeoutMs
      doOutput = true
      setRequestProperty("Content-Type", "application/json")
      setRequestProperty(HEADER_TIMESTAMP, ts.toString())
      setRequestProperty(HEADER_SIGNATURE, sign(secret, ts, raw))
    }
    try {
      conn.outputStream.use { it.write(raw.toByteArray(Charsets.UTF_8)) }
      val code = conn.responseCode
      val text = (if (code in 200..299) conn.inputStream else conn.errorStream)?.use { it.readBytes().toString(Charsets.UTF_8) } ?: ""
      if (code !in 200..299) {
        val reason = runCatching { JSONObject(text).optString("error") }.getOrNull().orEmpty()
        throw GatewayHttpException(code, "HTTP $code ${reason.take(120)}".trim())
      }
      return if (text.isBlank()) JSONObject() else JSONObject(text)
    } finally {
      conn.disconnect()
    }
  }

  companion object {
    const val HEADER_SIGNATURE = "X-Webhook-Signature"
    const val HEADER_TIMESTAMP = "X-Webhook-Timestamp"

    const val INBOUND = "/gateway/v1/inbound"
    const val OUTBOX = "/gateway/v1/outbox"
    const val ACK = "/gateway/v1/ack"
    const val STATUS = "/gateway/v1/status"
    const val HEARTBEAT = "/gateway/v1/heartbeat"

    /** "sha256=" + hex(HMAC-SHA256(secret, "<unix seconds>.<body>")). */
    fun sign(secret: String, timestamp: Long, body: String): String {
      val mac = Mac.getInstance("HmacSHA256")
      mac.init(SecretKeySpec(secret.toByteArray(Charsets.UTF_8), "HmacSHA256"))
      val digest = mac.doFinal("$timestamp.$body".toByteArray(Charsets.UTF_8))
      return "sha256=" + digest.joinToString("") { "%02x".format(it) }
    }
  }
}
