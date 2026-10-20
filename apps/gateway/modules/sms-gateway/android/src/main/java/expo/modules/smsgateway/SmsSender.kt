package expo.modules.smsgateway

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.os.Build
import android.telephony.SmsManager

/** Sends replies from the SIM; results arrive at SmsResultReceiver. */
object SmsSender {
  const val ACTION_SENT = "expo.modules.smsgateway.SMS_SENT"
  const val ACTION_DELIVERED = "expo.modules.smsgateway.SMS_DELIVERED"
  const val EXTRA_ID = "id"
  const val EXTRA_PART = "part"

  @Suppress("DEPRECATION")
  fun smsManager(context: Context, subscriptionId: Int): SmsManager {
    if (Build.VERSION.SDK_INT >= 31) {
      val base = context.getSystemService(SmsManager::class.java)
      return if (subscriptionId >= 0) base.createForSubscriptionId(subscriptionId) else base
    }
    return if (subscriptionId >= 0) SmsManager.getSmsManagerForSubscriptionId(subscriptionId) else SmsManager.getDefault()
  }

  /**
   * Sends one reply once. The row is claimed ('sending') before the radio is
   * touched, so a crash can never lead to a second copy.
   */
  fun send(context: Context, db: GatewayDb, row: GatewayDb.Outbound, subscriptionId: Int) {
    val sm = smsManager(context, subscriptionId)
    val parts = sm.divideMessage(row.body)
    if (!db.markSending(row.id, parts.size)) return
    try {
      val sent = ArrayList(parts.indices.map { pendingIntent(context, ACTION_SENT, row.id, it) })
      val delivered = ArrayList(parts.indices.map { pendingIntent(context, ACTION_DELIVERED, row.id, it) })
      if (parts.size == 1) {
        sm.sendTextMessage(row.recipient, null, row.body, sent[0], delivered[0])
      } else {
        sm.sendMultipartTextMessage(row.recipient, null, parts, sent, delivered)
      }
      db.addEvent("send", "Reply to ${mask(row.recipient)} (${parts.size} part${if (parts.size == 1) "" else "s"})")
    } catch (e: Exception) {
      // Thrown before anything left the phone: a definite failure.
      db.onPartSent(row.id, false, "send_exception: ${e.javaClass.simpleName}")
      db.addEvent("error", "Could not send to ${mask(row.recipient)}: ${e.javaClass.simpleName}")
    }
  }

  private fun pendingIntent(context: Context, action: String, id: String, part: Int): PendingIntent {
    val intent = Intent(context, SmsResultReceiver::class.java).setAction(action).putExtra(EXTRA_ID, id).putExtra(EXTRA_PART, part)
    // Mutable so the system can attach the error code / delivery-report PDU; the intent is explicit.
    val flags = PendingIntent.FLAG_UPDATE_CURRENT or (if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0)
    return PendingIntent.getBroadcast(context, "$action/$id/$part".hashCode(), intent, flags)
  }

  fun mask(number: String): String =
    if (number.length < 7) "***" else number.take(4) + "*".repeat(number.length - 6) + number.takeLast(2)
}
