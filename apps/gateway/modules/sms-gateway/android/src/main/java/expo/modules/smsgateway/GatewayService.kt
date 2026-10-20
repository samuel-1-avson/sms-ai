package expo.modules.smsgateway

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import expo.modules.smsgateway.brain.AiClient
import expo.modules.smsgateway.brain.Brain
import expo.modules.smsgateway.brain.Breaker
import expo.modules.smsgateway.brain.ChatMessage
import expo.modules.smsgateway.brain.DEEPINFRA_BASE_URL
import expo.modules.smsgateway.brain.DeepInfra
import expo.modules.smsgateway.brain.Outcome
import expo.modules.smsgateway.brain.StepResult

/**
 * The SMS AI itself: a foreground service that keeps two loops running while
 * the phone is on, whether or not the app's screen is open.
 *
 *  answerer  takes received texts, asks the AI (over the internet) and queues
 *            the replies
 *  sender    sends queued replies from the SIM, one at a time
 *
 * Both work from the local database, so they pick up where they left off
 * after the app is killed, the phone reboots or the internet drops.
 */
class GatewayService : Service() {
  @Volatile private var stopping = false
  private var answerer: Thread? = null
  private var sender: Thread? = null
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
    GatewayDb.get(this).resetAnswering()
    answerer = Thread({ answerLoop() }, "smsai-answerer").apply { start() }
    sender = Thread({ sendLoop() }, "smsai-sender").apply { start() }
    GatewayDb.get(this).addEvent("service", "SMS AI started")
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
    answerer?.interrupt()
    sender?.interrupt()
    wakeLock?.let { if (it.isHeld) it.release() }
    GatewayDb.get(this).addEvent("service", "SMS AI stopped")
    super.onDestroy()
  }

  // ---------------------------------------------------------------------------
  // Loops
  // ---------------------------------------------------------------------------

  private fun answerLoop() {
    val db = GatewayDb.get(this)
    val brain = brain(this)
    var lastPrune = 0L
    while (!stopping) {
      try {
        val prefs = GatewayPrefs(this)
        if (prefs.paused) {
          waitForWork(30_000)
          continue
        }
        if (!prefs.configured) {
          fail("Add your DeepInfra API key in Settings")
          waitForWork(30_000)
          continue
        }
        // Answer everyone who is ready; each step is one sender.
        while (!stopping && !GatewayPrefs(this).paused) {
          val result = brain.step()
          when (result) {
            StepResult.IDLE -> break
            StepResult.ANSWERED -> ok(prefs)
            StepResult.RETRY, StepResult.FAILED -> fail("AI: ${brain.lastError ?: "unavailable"}")
            else -> {}
          }
          if (result != StepResult.IDLE) wake()
        }
        if (System.currentTimeMillis() - lastPrune > 3_600_000L) {
          db.prune()
          lastPrune = System.currentTimeMillis()
        }
        // Sleep until a text arrives, or briefly while texts are waiting out
        // the burst window or a retry delay.
        val due = db.nextDueAt()
        val now = System.currentTimeMillis()
        waitForWork(
          when {
            due == null -> 60_000L
            due <= now -> 3_000L
            else -> (due - now).coerceIn(1_000L, 60_000L)
          },
        )
      } catch (e: InterruptedException) {
        return
      } catch (e: Exception) {
        fail("${e.javaClass.simpleName}: ${e.message ?: ""}".take(160))
        // This thread is the only one answering, so anything still claimed
        // was claimed by the step that just failed: let it be tried again.
        runCatching { db.resetAnswering() }
        try {
          Thread.sleep(5_000)
        } catch (_: InterruptedException) {
          return
        }
      }
    }
  }

  private fun sendLoop() {
    while (!stopping) {
      try {
        val prefs = GatewayPrefs(this)
        if (!prefs.paused) {
          val db = GatewayDb.get(this)
          for (row in db.pendingSends(50)) {
            if (stopping || GatewayPrefs(this).paused) break
            SmsSender.send(this, db, row, prefs.subscriptionId)
            Thread.sleep(prefs.sendIntervalMs)
          }
        }
        waitForWork(15_000)
      } catch (e: InterruptedException) {
        return
      } catch (e: Exception) {
        Log.w(TAG, "send loop: ${e.javaClass.simpleName}: ${e.message}")
        try {
          Thread.sleep(5_000)
        } catch (_: InterruptedException) {
          return
        }
      }
    }
  }

  private fun ok(prefs: GatewayPrefs) {
    prefs.lastAnswerAt = System.currentTimeMillis()
    if (prefs.lastError != null) {
      prefs.lastError = null
      GatewayDb.get(this).addEvent("service", "AI is answering again")
      updateNotification()
    }
  }

  private fun fail(msg: String) {
    val prefs = GatewayPrefs(this)
    if (prefs.lastError != msg) {
      prefs.lastError = msg
      updateNotification()
    }
    Log.w(TAG, msg)
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
      nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "SMS AI", NotificationManager.IMPORTANCE_LOW))
    }
    val error = GatewayPrefs(this).lastError
    val open = packageManager.getLaunchIntentForPackage(packageName)?.let {
      PendingIntent.getActivity(this, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }
    val b = if (Build.VERSION.SDK_INT >= 26) Notification.Builder(this, CHANNEL_ID) else Notification.Builder(this)
    return b.setContentTitle(if (error == null) "SMS AI is running" else "SMS AI: problem")
      .setContentText(error ?: "Answering texts to this phone")
      .setSmallIcon(android.R.drawable.stat_notify_chat)
      .setOngoing(true)
      .apply { if (open != null) setContentIntent(open) }
      .build()
  }

  companion object {
    private const val TAG = "SmsAi"
    private const val CHANNEL_ID = "smsai_gateway"
    private const val NOTIFICATION_ID = 7131
    private const val ACTION_STOP = "expo.modules.smsgateway.STOP"
    private val signal = Object()

    /** Shared by every AI call so a provider outage pauses calls app-wide. Replaced when the key changes. */
    @Volatile var breaker = Breaker()

    @Volatile var running = false
      private set

    /** Wakes both loops now (a text arrived, a reply was queued or a setting changed). */
    fun wake() {
      synchronized(signal) { signal.notifyAll() }
    }

    private fun waitForWork(ms: Long) {
      synchronized(signal) { signal.wait(ms.coerceAtLeast(1)) }
    }

    /** One AI call with the current settings (also used by the app's "try a question" box). */
    fun ask(context: Context, messages: List<ChatMessage>): Outcome {
      val prefs = GatewayPrefs(context)
      val key = prefs.apiKey ?: throw IllegalStateException("No API key")
      return AiClient(DeepInfra(DEEPINFRA_BASE_URL, key), prefs.aiOptions(), breaker).answer(messages)
    }

    fun brain(context: Context): Brain {
      val app = context.applicationContext
      return Brain(GatewayDb.get(app), { GatewayPrefs(app).brainSettings() }, { ask(app, it) })
    }

    fun start(context: Context) {
      val intent = Intent(context, GatewayService::class.java)
      try {
        if (Build.VERSION.SDK_INT >= 26) context.startForegroundService(intent) else context.startService(intent)
      } catch (e: Exception) {
        // Android can refuse a background start; the text is saved and is
        // answered when the service next runs (app opened, reboot, or a
        // START_STICKY restart).
        Log.w(TAG, "could not start the service: ${e.javaClass.simpleName}")
      }
    }

    fun stop(context: Context) {
      context.stopService(Intent(context, GatewayService::class.java))
    }
  }
}
