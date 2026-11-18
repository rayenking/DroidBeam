package com.droidbeam.gateway

import android.app.PendingIntent
import android.content.Context
import android.os.Build
import android.telephony.SmsManager

/** Sends SMS through the chosen SIM, tracking per-part results for multipart messages. */
object SmsSender {

    /** Sends the message and returns the SIM slot used (null = system default, unknown). */
    fun send(context: Context, msg: OutboxMessage): Int? {
        val sim = SimManager.choose(context, msg.sim)
        val sms = smsManager(context, sim?.subId)
        val parts = sms.divideMessage(msg.body)
        val total = parts.size
        val sentIntents = ArrayList<PendingIntent>(total)
        val deliveredIntents = ArrayList<PendingIntent>(total)
        for (i in 0 until total) {
            sentIntents += SmsStatusReceiver.pendingIntent(context, SmsStatusReceiver.ACTION_SENT, msg.id, i, sim?.slot)
            deliveredIntents += SmsStatusReceiver.pendingIntent(context, SmsStatusReceiver.ACTION_DELIVERED, msg.id, i, sim?.slot)
        }
        PartTracker.start(msg.id, total)
        if (total == 1) {
            sms.sendTextMessage(msg.to, null, parts[0], sentIntents[0], deliveredIntents[0])
        } else {
            sms.sendMultipartTextMessage(msg.to, null, parts, sentIntents, deliveredIntents)
        }
        return sim?.slot
    }

    private fun smsManager(context: Context, subId: Int?): SmsManager {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val base = context.getSystemService(SmsManager::class.java)
            return if (subId != null) base.createForSubscriptionId(subId) else base
        }
        @Suppress("DEPRECATION")
        return if (subId != null) SmsManager.getSmsManagerForSubscriptionId(subId) else SmsManager.getDefault()
    }
}

/**
 * Collapses per-part sent/delivery callbacks into one result per message.
 * State is in memory; if the process dies mid-send, each callback is treated as final.
 */
object PartTracker {
    enum class Outcome { PENDING, OK, FAILED, IGNORE }

    private class Entry(val total: Int, val startedAt: Long) {
        var sentOk = 0
        var sendFailed = false
        var delivered = 0
        var deliveryDone = false
    }

    private val entries = HashMap<Long, Entry>()
    private const val MAX_AGE_MS = 24 * 60 * 60 * 1000L

    @Synchronized
    fun start(id: Long, total: Int) {
        val now = System.currentTimeMillis()
        // Carriers often never send delivery reports; drop old entries so the map stays small.
        entries.entries.removeAll { now - it.value.startedAt > MAX_AGE_MS }
        entries[id] = Entry(total, now)
    }

    @Synchronized
    fun onSent(id: Long, ok: Boolean): Outcome {
        val e = entries[id] ?: return if (ok) Outcome.OK else Outcome.FAILED
        if (e.sendFailed) return Outcome.IGNORE
        if (!ok) {
            e.sendFailed = true
            return Outcome.FAILED
        }
        e.sentOk++
        return if (e.sentOk == e.total) Outcome.OK else Outcome.PENDING
    }

    @Synchronized
    fun onDelivered(id: Long, ok: Boolean): Outcome {
        val e = entries[id] ?: return if (ok) Outcome.OK else Outcome.FAILED
        if (e.sendFailed || e.deliveryDone) return Outcome.IGNORE
        if (!ok) {
            e.deliveryDone = true
            entries.remove(id)
            return Outcome.FAILED
        }
        e.delivered++
        if (e.delivered < e.total) return Outcome.PENDING
        entries.remove(id)
        return Outcome.OK
    }
}
