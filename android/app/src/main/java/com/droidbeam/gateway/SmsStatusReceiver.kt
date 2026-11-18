package com.droidbeam.gateway

import android.app.Activity
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.telephony.SmsManager
import android.telephony.SmsMessage

/** Receives the sent/delivered callbacks for SMS dispatched by [SmsSender]. */
class SmsStatusReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val id = intent.getLongExtra(EXTRA_MESSAGE_ID, -1)
        if (id < 0) return
        val sim = intent.getIntExtra(EXTRA_SIM, 0).takeIf { it > 0 }
        val simLabel = sim?.let { " (SIM $it)" } ?: ""
        when (intent.action) {
            ACTION_SENT -> {
                val ok = resultCode == Activity.RESULT_OK
                when (PartTracker.onSent(id, ok)) {
                    PartTracker.Outcome.OK -> {
                        ReportWorker.enqueueStatus(context, id, "sent", sim = sim)
                        GatewayState.countSent()
                        GatewayState.log("#$id terkirim$simLabel")
                    }
                    PartTracker.Outcome.FAILED -> {
                        val reason = sendErrorName(resultCode)
                        ReportWorker.enqueueStatus(context, id, "failed", reason, sim)
                        GatewayState.countFailed()
                        GatewayState.log("#$id gagal$simLabel: $reason")
                    }
                    else -> Unit
                }
            }
            ACTION_DELIVERED -> {
                val ok = deliveryResult(intent) ?: return // still pending at the carrier
                when (PartTracker.onDelivered(id, ok)) {
                    PartTracker.Outcome.OK -> {
                        ReportWorker.enqueueStatus(context, id, "delivered", sim = sim)
                        GatewayState.log("#$id diterima penerima")
                    }
                    PartTracker.Outcome.FAILED -> {
                        ReportWorker.enqueueStatus(context, id, "failed", "delivery failed", sim)
                        GatewayState.log("#$id gagal sampai ke penerima")
                    }
                    else -> Unit
                }
            }
        }
    }

    /** true = delivered, false = permanent failure, null = carrier still trying. */
    private fun deliveryResult(intent: Intent): Boolean? {
        val pdu = intent.getByteArrayExtra("pdu") ?: return true
        val format = intent.getStringExtra("format")
        if (format != null && format != SmsMessage.FORMAT_3GPP) return true
        val status = SmsMessage.createFromPdu(pdu, format)?.status ?: return true
        // GSM TP-Status: 0x00-0x1F completed, 0x20-0x3F temporary error (retrying), >= 0x40 failed.
        return when {
            status < 0x20 -> true
            status < 0x40 -> null
            else -> false
        }
    }

    private fun sendErrorName(code: Int): String = when (code) {
        SmsManager.RESULT_ERROR_GENERIC_FAILURE -> "generic failure"
        SmsManager.RESULT_ERROR_NO_SERVICE -> "no service"
        SmsManager.RESULT_ERROR_NULL_PDU -> "null PDU"
        SmsManager.RESULT_ERROR_RADIO_OFF -> "radio off"
        SmsManager.RESULT_ERROR_LIMIT_EXCEEDED -> "SMS limit exceeded"
        SmsManager.RESULT_ERROR_SHORT_CODE_NOT_ALLOWED -> "short code not allowed"
        SmsManager.RESULT_ERROR_SHORT_CODE_NEVER_ALLOWED -> "short code never allowed"
        else -> "error code $code"
    }

    companion object {
        const val ACTION_SENT = "com.droidbeam.gateway.SMS_SENT"
        const val ACTION_DELIVERED = "com.droidbeam.gateway.SMS_DELIVERED"
        private const val EXTRA_MESSAGE_ID = "message_id"
        private const val EXTRA_SIM = "sim"

        fun pendingIntent(context: Context, action: String, messageId: Long, part: Int, sim: Int?): PendingIntent {
            val intent = Intent(context, SmsStatusReceiver::class.java)
                .setAction(action)
                // Unique data URI so each part gets its own PendingIntent.
                .setData(Uri.parse("droidbeam://sms/$messageId/$part"))
                .putExtra(EXTRA_MESSAGE_ID, messageId)
                .putExtra(EXTRA_SIM, sim ?: 0)
            // Mutable so the telephony stack can attach the delivery-report PDU; the intent is explicit.
            return PendingIntent.getBroadcast(
                context, 0, intent,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_MUTABLE,
            )
        }
    }
}
