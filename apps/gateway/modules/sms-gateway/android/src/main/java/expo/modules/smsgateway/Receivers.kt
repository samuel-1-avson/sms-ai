package expo.modules.smsgateway

import android.app.Activity
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.provider.Telephony
import android.telephony.SmsManager
import android.telephony.SmsMessage
import java.util.UUID

/**
 * Incoming texts. Each one is written to the local inbox (with a fresh UUID
 * the server uses to drop retries) before anything else happens.
 */
class SmsReceiver : BroadcastReceiver() {
  override fun onReceive(context: Context, intent: Intent) {
    if (intent.action != Telephony.Sms.Intents.SMS_RECEIVED_ACTION) return
    val prefs = GatewayPrefs(context)
    if (!prefs.configured) return
    val sub = intent.getIntExtra("android.telephony.extra.SUBSCRIPTION_INDEX", intent.getIntExtra("subscription", -1))
    if (prefs.subscriptionId >= 0 && sub >= 0 && sub != prefs.subscriptionId) return

    val messages = Telephony.Sms.Intents.getMessagesFromIntent(intent) ?: return
    val db = GatewayDb.get(context)
    // One broadcast can hold the parts of a long text (and, rarely, several senders).
    messages.filterNotNull().groupBy { it.displayOriginatingAddress ?: it.originatingAddress ?: "" }.forEach { (sender, parts) ->
      if (sender.isBlank()) return@forEach
      db.insertInbound(
        GatewayDb.Inbound(
          id = UUID.randomUUID().toString(),
          sender = sender,
          body = parts.joinToString("") { it.displayMessageBody ?: it.messageBody ?: "" },
          receivedAt = parts.first().timestampMillis.takeIf { it > 0 } ?: System.currentTimeMillis(),
          sim = if (sub >= 0) sub.toString() else null,
        ),
      )
      db.addEvent("receive", "Text from ${SmsSender.mask(sender)}")
    }
    GatewayService.wake()
    if (prefs.enabled) GatewayService.start(context)
  }
}

/** Send results and delivery reports for the replies we sent. */
class SmsResultReceiver : BroadcastReceiver() {
  override fun onReceive(context: Context, intent: Intent) {
    val id = intent.getStringExtra(SmsSender.EXTRA_ID) ?: return
    val db = GatewayDb.get(context)
    when (intent.action) {
      SmsSender.ACTION_SENT -> {
        val ok = resultCode == Activity.RESULT_OK
        db.onPartSent(id, ok, if (ok) null else sendError(resultCode))
        if (!ok) db.addEvent("error", "A reply failed to send: ${sendError(resultCode)}")
      }
      SmsSender.ACTION_DELIVERED -> {
        val pdu = intent.getByteArrayExtra("pdu") ?: return
        val status = SmsMessage.createFromPdu(pdu, intent.getStringExtra("format"))?.status ?: return
        // TP-Status: 0-31 delivered, 32-63 still trying, 64+ failed.
        when {
          status < 32 -> db.onPartDelivered(id, true, null)
          status >= 64 -> db.onPartDelivered(id, false, "delivery_failed_$status")
        }
      }
      else -> return
    }
    GatewayService.wake()
  }

  private fun sendError(code: Int): String = when (code) {
    SmsManager.RESULT_ERROR_GENERIC_FAILURE -> "generic_failure"
    SmsManager.RESULT_ERROR_NO_SERVICE -> "no_service"
    SmsManager.RESULT_ERROR_NULL_PDU -> "null_pdu"
    SmsManager.RESULT_ERROR_RADIO_OFF -> "radio_off"
    else -> "error_$code"
  }
}

/** Restarts the gateway after a reboot or an app update, if it was running. */
class BootReceiver : BroadcastReceiver() {
  override fun onReceive(context: Context, intent: Intent) {
    if (intent.action == Intent.ACTION_BOOT_COMPLETED || intent.action == Intent.ACTION_MY_PACKAGE_REPLACED) {
      if (GatewayPrefs(context).enabled) GatewayService.start(context)
    }
  }
}
