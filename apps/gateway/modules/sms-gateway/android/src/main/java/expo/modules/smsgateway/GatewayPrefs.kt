package expo.modules.smsgateway

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.security.KeyStore
import java.util.UUID
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * Gateway settings. The device secret is sealed with an AES key that lives in
 * the Android Keystore, so it never sits in plain text on disk; everything
 * else is ordinary app-private preferences.
 */
class GatewayPrefs(context: Context) {
  private val sp = context.applicationContext.getSharedPreferences("smsai_gateway", Context.MODE_PRIVATE)

  var serverUrl: String?
    get() = sp.getString("server_url", null)
    set(v) = sp.edit().putString("server_url", v).apply()

  var secret: String?
    get() = sp.getString("secret_sealed", null)?.let { runCatching { open(it) }.getOrNull() }
    set(v) = sp.edit().putString("secret_sealed", v?.let { seal(it) }).apply()

  /** The service should run (set by the user, survives reboots). */
  var enabled: Boolean
    get() = sp.getBoolean("enabled", false)
    set(v) = sp.edit().putBoolean("enabled", v).apply()

  /** Replies are not collected or sent while paused; received texts are still forwarded. */
  var paused: Boolean
    get() = sp.getBoolean("paused", false)
    set(v) = sp.edit().putBoolean("paused", v).apply()

  /** The SIM that answers, or -1 for the default SMS SIM. Texts to other SIMs are ignored. */
  var subscriptionId: Int
    get() = sp.getInt("subscription_id", -1)
    set(v) = sp.edit().putInt("subscription_id", v).apply()

  /** Gap between two outgoing SMS, to stay clear of carrier and Android rate limits. */
  var sendIntervalMs: Long
    get() = sp.getLong("send_interval_ms", 2000L)
    set(v) = sp.edit().putLong("send_interval_ms", v.coerceIn(0L, 60_000L)).apply()

  val deviceId: String
    get() = sp.getString("device_id", null) ?: UUID.randomUUID().toString().also { sp.edit().putString("device_id", it).apply() }

  var lastError: String?
    get() = sp.getString("last_error", null)
    set(v) = sp.edit().putString("last_error", v).apply()

  var lastContactAt: Long
    get() = sp.getLong("last_contact_at", 0L)
    set(v) = sp.edit().putLong("last_contact_at", v).apply()

  val configured: Boolean
    get() = !serverUrl.isNullOrBlank() && !secret.isNullOrBlank()

  fun clear() {
    sp.edit().remove("server_url").remove("secret_sealed").putBoolean("enabled", false).apply()
  }

  private fun key(): SecretKey {
    val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
    (ks.getKey(KEY_ALIAS, null) as? SecretKey)?.let { return it }
    val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
    gen.init(
      KeyGenParameterSpec.Builder(KEY_ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
        .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
        .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
        .setKeySize(256)
        .build(),
    )
    return gen.generateKey()
  }

  private fun seal(plain: String): String {
    val cipher = Cipher.getInstance("AES/GCM/NoPadding")
    cipher.init(Cipher.ENCRYPT_MODE, key())
    val ct = cipher.doFinal(plain.toByteArray(Charsets.UTF_8))
    return b64(cipher.iv) + ":" + b64(ct)
  }

  private fun open(sealed: String): String {
    val (iv, ct) = sealed.split(":", limit = 2)
    val cipher = Cipher.getInstance("AES/GCM/NoPadding")
    cipher.init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, Base64.decode(iv, Base64.NO_WRAP)))
    return String(cipher.doFinal(Base64.decode(ct, Base64.NO_WRAP)), Charsets.UTF_8)
  }

  private fun b64(b: ByteArray) = Base64.encodeToString(b, Base64.NO_WRAP)

  companion object {
    private const val KEY_ALIAS = "smsai_gateway_secret"
  }
}
