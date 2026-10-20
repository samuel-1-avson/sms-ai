package expo.modules.smsgateway

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import android.telephony.SubscriptionManager
import expo.modules.kotlin.exception.CodedException
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition
import expo.modules.kotlin.records.Field
import expo.modules.kotlin.records.Record
import expo.modules.smsgateway.brain.AiError
import expo.modules.smsgateway.brain.AnswerError
import expo.modules.smsgateway.brain.Breaker
import expo.modules.smsgateway.brain.Brain
import expo.modules.smsgateway.brain.buildPrompt
import expo.modules.smsgateway.brain.sanitize
import expo.modules.smsgateway.brain.scrub
import expo.modules.smsgateway.brain.splitFirst
import expo.modules.smsgateway.brain.truncateQuestion
import expo.modules.smsgateway.brain.MAX_QUESTION_CHARS

/** Settings the app's Settings screen can change; fields left out are kept. */
class SettingsUpdate : Record {
  @Field var model: String? = null
  @Field var fallbackModel: String? = null
  @Field var ownNumber: String? = null
  @Field var dailyPerNumberCap: Int? = null
  @Field var dailySmsPartCap: Int? = null
  @Field var dailySpendCapUsd: Double? = null
  @Field var subscriptionId: Int? = null
  @Field var sendIntervalMs: Double? = null
}

/** The JavaScript face of the SMS AI: settings, start/stop, status and a test question. */
class SmsGatewayModule : Module() {
  private val context: Context
    get() = appContext.reactContext ?: throw CodedException("ERR_NO_CONTEXT", "React context is not available", null)

  override fun definition() = ModuleDefinition {
    Name("SmsGateway")

    Function("getStatus") {
      val prefs = GatewayPrefs(context)
      val db = GatewayDb.get(context)
      val counts = db.counts()
      val usage = db.usage(Brain.dayOf(System.currentTimeMillis()))
      mapOf(
        "configured" to prefs.configured,
        "enabled" to prefs.enabled,
        "running" to GatewayService.running,
        "paused" to prefs.paused,
        "lastError" to prefs.lastError,
        "lastAnswerAt" to prefs.lastAnswerAt.toDouble(),
        "aiBreaker" to GatewayService.breaker.state(),
        "waiting" to counts.waiting,
        "outboxPending" to counts.outboxPending,
        "sending" to counts.sending,
        "sentToday" to counts.sentToday,
        "failedToday" to counts.failedToday,
        "receivedToday" to counts.receivedToday,
        "answeredToday" to usage.questions,
        "smsPartsToday" to usage.smsParts,
        "aiCostToday" to usage.aiCostUsd,
        "aiCallsToday" to usage.aiCalls,
        "aiFailuresToday" to usage.aiFailures,
      )
    }

    Function("getSettings") {
      val prefs = GatewayPrefs(context)
      val key = prefs.apiKey
      mapOf(
        "hasApiKey" to !key.isNullOrBlank(),
        // Enough to recognise which key is saved, never the key itself.
        "apiKeyHint" to key?.takeIf { it.length >= 8 }?.let { "…" + it.takeLast(4) },
        "model" to prefs.model,
        "fallbackModel" to prefs.fallbackModel,
        "ownNumber" to prefs.ownNumber,
        "dailyPerNumberCap" to prefs.dailyPerNumberCap,
        "dailySmsPartCap" to prefs.dailySmsPartCap,
        "dailySpendCapUsd" to prefs.dailySpendCapUsd,
        "subscriptionId" to prefs.subscriptionId,
        "sendIntervalMs" to prefs.sendIntervalMs.toDouble(),
      )
    }

    Function("setSettings") { s: SettingsUpdate ->
      val prefs = GatewayPrefs(context)
      s.model?.let { if (it.isNotBlank()) prefs.model = it }
      s.fallbackModel?.let { prefs.fallbackModel = it }
      s.ownNumber?.let { prefs.ownNumber = it }
      s.dailyPerNumberCap?.let { prefs.dailyPerNumberCap = it }
      s.dailySmsPartCap?.let { prefs.dailySmsPartCap = it }
      s.dailySpendCapUsd?.let { prefs.dailySpendCapUsd = it }
      s.subscriptionId?.let { prefs.subscriptionId = it }
      s.sendIntervalMs?.let { prefs.sendIntervalMs = it.toLong() }
      GatewayService.wake()
    }

    Function("setApiKey") { key: String ->
      val k = key.trim()
      if (k.length < 16 || k.any { it.isWhitespace() }) throw CodedException("ERR_KEY", "That does not look like a DeepInfra API key", null)
      val prefs = GatewayPrefs(context)
      prefs.apiKey = k
      prefs.lastError = null
      // A new key deserves a fresh start, even if the old one tripped the breaker.
      GatewayService.breaker = Breaker()
      GatewayDb.get(context).addEvent("service", "API key saved")
      GatewayService.wake()
    }

    Function("clearApiKey") {
      GatewayPrefs(context).apiKey = null
      GatewayDb.get(context).addEvent("service", "API key removed")
    }

    Function("getPermissions") {
      fun granted(p: String) = context.checkSelfPermission(p) == PackageManager.PERMISSION_GRANTED
      val pm = context.getSystemService(Context.POWER_SERVICE) as PowerManager
      mapOf(
        "receiveSms" to granted(Manifest.permission.RECEIVE_SMS),
        "sendSms" to granted(Manifest.permission.SEND_SMS),
        "readPhoneState" to granted(Manifest.permission.READ_PHONE_STATE),
        "notifications" to (Build.VERSION.SDK_INT < 33 || granted("android.permission.POST_NOTIFICATIONS")),
        "ignoringBatteryOptimizations" to pm.isIgnoringBatteryOptimizations(context.packageName),
      )
    }

    Function("start") {
      val prefs = GatewayPrefs(context)
      if (!prefs.configured) throw CodedException("ERR_NO_KEY", "Add your DeepInfra API key first", null)
      prefs.enabled = true
      GatewayService.start(context)
    }

    Function("stop") {
      GatewayPrefs(context).enabled = false
      GatewayService.stop(context)
    }

    Function("setPaused") { paused: Boolean ->
      GatewayPrefs(context).paused = paused
      GatewayDb.get(context).addEvent("service", if (paused) "Replies paused" else "Replies resumed")
      GatewayService.wake()
    }

    /** Active SIMs (needs READ_PHONE_STATE). */
    Function("listSims") {
      val sm = context.getSystemService(SubscriptionManager::class.java)
      val subs = try {
        sm.activeSubscriptionInfoList ?: emptyList()
      } catch (_: SecurityException) {
        emptyList()
      }
      subs.map {
        mapOf(
          "subscriptionId" to it.subscriptionId,
          "slot" to it.simSlotIndex,
          "label" to (it.displayName?.toString() ?: ""),
          "carrier" to (it.carrierName?.toString() ?: ""),
        )
      }
    }

    Function("recentEvents") { limit: Int ->
      GatewayDb.get(context).recentEvents(limit.coerceIn(1, 200)).map { mapOf("at" to it.at.toDouble(), "kind" to it.kind, "text" to it.text) }
    }

    /**
     * Asks the AI one question exactly as a texter would (same prompt, same
     * clean-up, same first-SMS cut) but sends nothing. Proves the key works.
     */
    AsyncFunction("askAI") { question: String ->
      val started = System.currentTimeMillis()
      try {
        val q = truncateQuestion(scrub(question), MAX_QUESTION_CHARS)
        val prefs = GatewayPrefs(context)
        val out = GatewayService.ask(context, buildPrompt(q, emptyList(), prefs.brainSettings().contextMaxInputTokens, System.currentTimeMillis()))
        val text = sanitize(out.text)
        val (first, rest) = splitFirst(text)
        mapOf(
          "ok" to true,
          "text" to text,
          "firstSms" to first,
          "hasMore" to rest.isNotEmpty(),
          "model" to out.model,
          "latencyMs" to (System.currentTimeMillis() - started).toDouble(),
          "costUsd" to out.calls.sumOf { it.costUsd },
          "error" to null,
        )
      } catch (e: AnswerError) {
        mapOf("ok" to false, "error" to ((e.error as? AiError)?.message ?: e.message ?: "AI error").removePrefix("ai: "))
      } catch (e: Exception) {
        mapOf("ok" to false, "error" to (e.message ?: e.javaClass.simpleName))
      }
    }

    Function("requestIgnoreBatteryOptimizations") {
      val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:${context.packageName}"))
        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
      context.startActivity(intent)
    }

    Function("openAppSettings") {
      val intent = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${context.packageName}"))
        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
      context.startActivity(intent)
    }
  }
}
