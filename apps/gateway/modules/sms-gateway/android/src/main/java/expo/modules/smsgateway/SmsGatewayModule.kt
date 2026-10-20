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

/** The JavaScript face of the gateway: pairing, start/stop and status for the app's screens. */
class SmsGatewayModule : Module() {
  private val context: Context
    get() = appContext.reactContext ?: throw CodedException("ERR_NO_CONTEXT", "React context is not available", null)

  override fun definition() = ModuleDefinition {
    Name("SmsGateway")

    Function("getStatus") {
      val prefs = GatewayPrefs(context)
      val counts = GatewayDb.get(context).counts()
      mapOf(
        "configured" to prefs.configured,
        "serverUrl" to prefs.serverUrl,
        "enabled" to prefs.enabled,
        "running" to GatewayService.running,
        "paused" to prefs.paused,
        "subscriptionId" to prefs.subscriptionId,
        "sendIntervalMs" to prefs.sendIntervalMs,
        "deviceId" to prefs.deviceId,
        "lastError" to prefs.lastError,
        "lastContactAt" to prefs.lastContactAt.toDouble(),
        "inboxPending" to counts.inboxPending,
        "outboxPending" to counts.outboxPending,
        "sending" to counts.sending,
        "statusPending" to counts.statusPending,
        "sentToday" to counts.sentToday,
        "receivedToday" to counts.receivedToday,
      )
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

    /** Saves the pairing (URL + device secret). Does not start the service. */
    Function("configure") { serverUrl: String, secret: String ->
      val url = serverUrl.trim().trimEnd('/')
      if (!url.startsWith("https://") && !url.startsWith("http://")) throw CodedException("ERR_URL", "Server URL must start with https://", null)
      if (secret.length < 24) throw CodedException("ERR_SECRET", "Device secret is too short", null)
      val prefs = GatewayPrefs(context)
      prefs.serverUrl = url
      prefs.secret = secret
      prefs.lastError = null
      GatewayDb.get(context).addEvent("service", "Paired with $url")
    }

    Function("unpair") {
      GatewayService.stop(context)
      GatewayPrefs(context).clear()
      GatewayDb.get(context).addEvent("service", "Unpaired")
    }

    Function("start") {
      val prefs = GatewayPrefs(context)
      if (!prefs.configured) throw CodedException("ERR_NOT_PAIRED", "Pair with the server first", null)
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

    Function("setSubscriptionId") { id: Int ->
      GatewayPrefs(context).subscriptionId = id
    }

    Function("setSendIntervalMs") { ms: Double ->
      GatewayPrefs(context).sendIntervalMs = ms.toLong()
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

    /** One signed heartbeat: proves the URL and secret before the service is started. */
    AsyncFunction("testConnection") { serverUrl: String, secret: String ->
      try {
        val res = GatewayClient(serverUrl.trim().trimEnd('/'), secret).post(GatewayClient.HEARTBEAT, GatewayService.heartbeatBody(context))
        mapOf("ok" to true, "serverTime" to res.optString("server_time"), "error" to null)
      } catch (e: GatewayHttpException) {
        mapOf("ok" to false, "error" to if (e.status == 401) "The server rejected the device secret (401)" else e.message)
      } catch (e: Exception) {
        mapOf("ok" to false, "error" to "${e.javaClass.simpleName}: ${e.message ?: "cannot reach the server"}")
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
