package expo.modules.smsgateway

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.BatteryManager
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.telephony.SubscriptionManager
import android.telephony.TelephonyManager
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.TimeZone

/**
 * The gateway itself: a foreground service that keeps two loops running
 * while the phone is on, whether or not the app's screen is open.
 *
 *  uploader  forwards received texts and send results, and sends a heartbeat
 *            every minute
 *  poller    long-polls the server for replies, stores them, acknowledges
 *            them and sends them from the SIM, one at a time
 *
 * Both loops back off when the network or the server is unavailable and
 * pick up where they left off from the local database.
 */
class GatewayService : Service() {
  @Volatile private var stopping = false
  private var uploader: Thread? = null
  private var poller: Thread? = null
  private var wakeLock: PowerManager.WakeLock? = null

  override fun onBind(intent: Intent?): IBinder? = null

  override fun onCreate() {
    super.onCreate()
    running = true
    startInForeground()
    // The phone is meant to stay plugged in: a partial wake lock keeps the
    // loops alive with the screen off.
    wakeLock = (getSystemService(Context.POWER_SERVICE) as PowerManager)
      .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "smsai:gateway")
      .apply { setReferenceCounted(false); acquire() }
    uploader = Thread({ uploadLoop() }, "smsai-uploader").apply { start() }
    poller = Thread({ pollLoop() }, "smsai-poller").apply { start() }
    GatewayDb.get(this).addEvent("service", "Gateway started")
  }

  override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
    if (intent?.action == ACTION_STOP) {
      stopSelf()
      return START_NOT_STICKY
    }
    startInForeground()
    wake()
    return START_STICKY
  }

  override fun onDestroy() {
    stopping = true
    running = false
    wake()
    uploader?.interrupt()
    poller?.interrupt()
    wakeLock?.let { if (it.isHeld) it.release() }
    GatewayDb.get(this).addEvent("service", "Gateway stopped")
    super.onDestroy()
  }

  // ---------------------------------------------------------------------------
  // Loops
  // ---------------------------------------------------------------------------

  private fun client(): GatewayClient? {
    val prefs = GatewayPrefs(this)
    val url = prefs.serverUrl ?: return null
    val secret = prefs.secret ?: return null
    return GatewayClient(url, secret)
  }

  private fun uploadLoop() {
    var backoff = 0L
    var lastHeartbeat = 0L
    var lastPrune = 0L
    while (!stopping) {
      try {
        val c = client() ?: throw IllegalStateException("not paired")
        flushInbox(c)
        flushStatuses(c)
        if (System.currentTimeMillis() - lastHeartbeat > HEARTBEAT_MS) {
          heartbeat(c)
          lastHeartbeat = System.currentTimeMillis()
        }
        if (System.currentTimeMillis() - lastPrune > 3_600_000L) {
          GatewayDb.get(this).prune()
          lastPrune = System.currentTimeMillis()
        }
        ok()
        backoff = 0
        waitForWork(15_000)
      } catch (e: InterruptedException) {
        return
      } catch (e: Exception) {
        backoff = nextBackoff(backoff)
        fail("upload", e)
        try {
          waitForWork(backoff)
        } catch (_: InterruptedException) {
          return
        }
      }
    }
  }

  private fun pollLoop() {
    var backoff = 0L
    while (!stopping) {
      try {
        val prefs = GatewayPrefs(this)
        if (prefs.paused) {
          Thread.sleep(5_000)
          continue
        }
        val c = client() ?: throw IllegalStateException("not paired")
        sendPending(prefs)
        val res = c.post(GatewayClient.OUTBOX, JSONObject().put("wait", 25), readTimeoutMs = 45_000)
        val arr = res.optJSONArray("messages") ?: JSONArray()
        if (arr.length() > 0) {
          val rows = (0 until arr.length()).map { i ->
            val o = arr.getJSONObject(i)
            GatewayDb.Outbound(o.getString("id"), o.getString("to"), o.getString("body"))
          }
          // Saved first, then acknowledged: an unacknowledged reply is offered
          // again, and the local primary key turns the repeat into a no-op.
          GatewayDb.get(this).insertOutbound(rows)
          c.post(GatewayClient.ACK, JSONObject().put("ids", JSONArray(rows.map { it.id })))
          sendPending(prefs)
        }
        ok()
        backoff = 0
      } catch (e: InterruptedException) {
        return
      } catch (e: Exception) {
        backoff = nextBackoff(backoff)
        fail("poll", e)
        try {
          Thread.sleep(backoff)
        } catch (_: InterruptedException) {
          return
        }
      }
    }
  }

  private fun sendPending(prefs: GatewayPrefs) {
    val db = GatewayDb.get(this)
    for (row in db.pendingSends(50)) {
      if (stopping || prefs.paused) return
      SmsSender.send(this, db, row, prefs.subscriptionId)
      Thread.sleep(prefs.sendIntervalMs)
    }
  }

  private fun flushInbox(c: GatewayClient) {
    val db = GatewayDb.get(this)
    while (true) {
      val batch = db.pendingInbound(50)
      if (batch.isEmpty()) return
      val messages = JSONArray()
      for (m in batch) {
        messages.put(
          JSONObject().put("id", m.id).put("from", m.sender).put("body", m.body).put("received_at", iso(m.receivedAt))
            .apply { if (m.sim != null) put("sim", m.sim) },
        )
      }
      c.post(GatewayClient.INBOUND, JSONObject().put("messages", messages))
      db.markUploaded(batch.map { it.id })
      if (batch.size < 50) return
    }
  }

  private fun flushStatuses(c: GatewayClient) {
    val db = GatewayDb.get(this)
    while (true) {
      val batch = db.pendingStatuses(50)
      if (batch.isEmpty()) return
      val statuses = JSONArray()
      for (s in batch) {
        statuses.put(
          JSONObject().put("id", s.id).put("status", s.status).put("at", iso(s.at)).apply { if (s.error != null) put("error", s.error.take(200)) },
        )
      }
      c.post(GatewayClient.STATUS, JSONObject().put("statuses", statuses))
      db.deleteStatusesThrough(batch.last().seq)
      if (batch.size < 50) return
    }
  }

  private fun heartbeat(c: GatewayClient) {
    c.post(GatewayClient.HEARTBEAT, heartbeatBody(this))
  }

  private fun ok() {
    val prefs = GatewayPrefs(this)
    prefs.lastContactAt = System.currentTimeMillis()
    if (prefs.lastError != null) {
      prefs.lastError = null
      GatewayDb.get(this).addEvent("service", "Connected to the server")
      updateNotification()
    }
  }

  private fun fail(where: String, e: Exception) {
    val msg = when (e) {
      is GatewayHttpException -> if (e.status == 401) "Server refused the pairing (401): pair again" else e.message ?: "HTTP ${e.status}"
      is IllegalStateException -> e.message ?: "not ready"
      else -> "${e.javaClass.simpleName}: ${e.message ?: ""}".take(160)
    }
    val prefs = GatewayPrefs(this)
    if (prefs.lastError != msg) {
      prefs.lastError = msg
      GatewayDb.get(this).addEvent("error", "$where: $msg")
      updateNotification()
    }
    Log.w(TAG, "$where failed: $msg")
  }

  // ---------------------------------------------------------------------------
  // Foreground notification
  // ---------------------------------------------------------------------------

  private fun startInForeground() {
    val n = notification()
    // The remoteMessaging type exists from Android 14; earlier versions take the type from the manifest.
    if (Build.VERSION.SDK_INT >= 34) {
      startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING)
    } else {
      startForeground(NOTIFICATION_ID, n)
    }
  }

  private fun updateNotification() {
    (getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager).notify(NOTIFICATION_ID, notification())
  }

  @Suppress("DEPRECATION")
  private fun notification(): Notification {
    val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
    if (Build.VERSION.SDK_INT >= 26 && nm.getNotificationChannel(CHANNEL_ID) == null) {
      nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "SMS gateway", NotificationManager.IMPORTANCE_LOW))
    }
    val error = GatewayPrefs(this).lastError
    val open = packageManager.getLaunchIntentForPackage(packageName)?.let {
      PendingIntent.getActivity(this, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }
    val b = if (Build.VERSION.SDK_INT >= 26) Notification.Builder(this, CHANNEL_ID) else Notification.Builder(this)
    return b.setContentTitle(if (error == null) "SMS AI gateway is running" else "SMS AI gateway: problem")
      .setContentText(error ?: "Answering texts to this phone")
      .setSmallIcon(android.R.drawable.stat_notify_chat)
      .setOngoing(true)
      .apply { if (open != null) setContentIntent(open) }
      .build()
  }

  companion object {
    private const val TAG = "SmsAiGateway"
    private const val CHANNEL_ID = "smsai_gateway"
    private const val NOTIFICATION_ID = 7131
    private const val ACTION_STOP = "expo.modules.smsgateway.STOP"
    private const val HEARTBEAT_MS = 60_000L
    private val signal = Object()

    @Volatile var running = false
      private set

    /** Wakes the uploader now (a text arrived or a send result came in). */
    fun wake() {
      synchronized(signal) { signal.notifyAll() }
    }

    private fun waitForWork(ms: Long) {
      synchronized(signal) { signal.wait(ms.coerceAtLeast(1)) }
    }

    private fun nextBackoff(prev: Long) = if (prev == 0L) 2_000L else (prev * 2).coerceAtMost(60_000L)

    fun start(context: Context) {
      val intent = Intent(context, GatewayService::class.java)
      try {
        if (Build.VERSION.SDK_INT >= 26) context.startForegroundService(intent) else context.startService(intent)
      } catch (e: Exception) {
        // Android can refuse a background start; the text is saved and goes out
        // when the service next runs (app opened, reboot, or START_STICKY restart).
        Log.w(TAG, "could not start the gateway service: ${e.javaClass.simpleName}")
      }
    }

    fun stop(context: Context) {
      context.stopService(Intent(context, GatewayService::class.java))
    }

    private fun iso(ms: Long): String =
      SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss.SSS'Z'", Locale.US).apply { timeZone = TimeZone.getTimeZone("UTC") }.format(Date(ms))

    /** The heartbeat body (also used by the module's connection test). */
    fun heartbeatBody(context: Context): JSONObject {
      val prefs = GatewayPrefs(context)
      val counts = GatewayDb.get(context).counts()
      val bm = context.getSystemService(Context.BATTERY_SERVICE) as BatteryManager
      val battery = bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY).takeIf { it in 0..100 }
      val charging = if (Build.VERSION.SDK_INT >= 23) bm.isCharging else null
      val signalLevel = runCatching {
        if (Build.VERSION.SDK_INT >= 28) (context.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager).signalStrength?.level else null
      }.getOrNull()
      val sim = runCatching {
        val sm = context.getSystemService(SubscriptionManager::class.java)
        val id = if (prefs.subscriptionId >= 0) prefs.subscriptionId else SubscriptionManager.getDefaultSmsSubscriptionId()
        sm.getActiveSubscriptionInfo(id)?.carrierName?.toString()
      }.getOrNull()
      val version = runCatching { context.packageManager.getPackageInfo(context.packageName, 0).versionName }.getOrNull() ?: ""
      return JSONObject()
        .put("device_id", prefs.deviceId)
        .put("app_version", version.take(32))
        .put("battery", battery ?: JSONObject.NULL)
        .put("charging", charging ?: JSONObject.NULL)
        .put("signal", signalLevel ?: JSONObject.NULL)
        .put("sim_label", sim?.take(64) ?: JSONObject.NULL)
        .put("inbox_pending", counts.inboxPending)
        .put("outbox_pending", counts.outboxPending + counts.sending)
        .put("status_pending", counts.statusPending)
        .put("paused", prefs.paused)
    }
  }
}
