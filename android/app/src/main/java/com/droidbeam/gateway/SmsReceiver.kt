package com.droidbeam.gateway

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.provider.Telephony
import java.util.UUID

/** Forwards incoming SMS to the server while the gateway is enabled. */
class SmsReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Telephony.Sms.Intents.SMS_RECEIVED_ACTION) return
        val prefs = Prefs(context)
        if (!prefs.isRegistered || !prefs.enabled) return

        val pdus = Telephony.Sms.Intents.getMessagesFromIntent(intent) ?: return
        val sim = SimManager.slotFromIntent(context, intent)
        // A long SMS arrives as several PDUs in the same broadcast; join them per sender.
        pdus.groupBy { it.originatingAddress ?: "unknown" }.forEach { (from, parts) ->
            val body = parts.joinToString("") { it.messageBody.orEmpty() }
            val receivedAt = parts.first().timestampMillis
            // Deterministic id lets the server drop duplicates if the upload is retried.
            val uid = UUID.nameUUIDFromBytes("$from|$receivedAt|$body".toByteArray()).toString()
            ReportWorker.enqueueIncoming(context, uid, from, body, receivedAt, sim)
            GatewayState.countReceived()
            GatewayState.log("SMS masuk dari $from${sim?.let { " (SIM $it)" } ?: ""}")
        }
    }
}
