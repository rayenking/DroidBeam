package com.droidbeam.gateway

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.telephony.ServiceState
import android.telephony.SubscriptionManager
import android.telephony.TelephonyManager
import androidx.core.content.ContextCompat
import org.json.JSONArray
import org.json.JSONObject

/** Dual-SIM support: lists active SIMs and picks which one sends each SMS. */
object SimManager {

    data class Sim(
        val slot: Int, // 1-based: SIM 1, SIM 2
        val subId: Int,
        val carrier: String,
        val inService: Boolean,
        val defaultSms: Boolean,
    ) {
        fun describe() = "SIM $slot · $carrier · ${if (inService) "in service" else "no service"}" +
            if (defaultSms) " · default SMS" else ""
    }

    fun hasPermission(context: Context) =
        ContextCompat.checkSelfPermission(context, Manifest.permission.READ_PHONE_STATE) ==
            PackageManager.PERMISSION_GRANTED

    @SuppressLint("MissingPermission") // checked by hasPermission
    fun list(context: Context): List<Sim> {
        if (!hasPermission(context)) return emptyList()
        val sm = context.getSystemService(SubscriptionManager::class.java) ?: return emptyList()
        val tm = context.getSystemService(TelephonyManager::class.java)
        val defaultSms = SubscriptionManager.getDefaultSmsSubscriptionId()
        val infos = runCatching { sm.activeSubscriptionInfoList.orEmpty() }.getOrDefault(emptyList())
        return infos.map { info ->
            val state = runCatching { tm?.createForSubscriptionId(info.subscriptionId)?.serviceState?.state }.getOrNull()
            Sim(
                slot = info.simSlotIndex + 1,
                subId = info.subscriptionId,
                carrier = info.displayName?.toString()?.ifBlank { null } ?: info.carrierName?.toString().orEmpty(),
                // Unknown state (OEM restriction) counts as usable rather than ruling the SIM out.
                inService = state == null || state == ServiceState.STATE_IN_SERVICE,
                defaultSms = info.subscriptionId == defaultSms,
            )
        }.sortedBy { it.slot }
    }

    /**
     * SIM to send through. An explicitly requested slot is always honoured.
     * Otherwise: the default SMS SIM if it has service, else any SIM with service.
     * Returns null when SIMs cannot be listed (no permission): the system default is used.
     */
    fun choose(context: Context, requestedSlot: Int?): Sim? {
        val sims = list(context)
        if (sims.isEmpty()) return null
        if (requestedSlot != null) {
            return sims.firstOrNull { it.slot == requestedSlot }
                ?: throw IllegalArgumentException("SIM $requestedSlot is not installed")
        }
        val default = sims.firstOrNull { it.defaultSms }
        if (default != null && default.inService) return default
        return sims.firstOrNull { it.inService } ?: default ?: sims.first()
    }

    /** Slot an incoming SMS broadcast arrived on, if the system says so. */
    fun slotFromIntent(context: Context, intent: Intent): Int? {
        val subId = intent.getIntExtra(SubscriptionManager.EXTRA_SUBSCRIPTION_INDEX, -1)
            .takeIf { it >= 0 } ?: intent.getIntExtra("subscription", -1)
        if (subId >= 0) list(context).firstOrNull { it.subId == subId }?.let { return it.slot }
        val slotIndex = intent.getIntExtra(SubscriptionManager.EXTRA_SLOT_INDEX, intent.getIntExtra("slot", -1))
        return if (slotIndex >= 0) slotIndex + 1 else null
    }

    fun toJson(sims: List<Sim>): JSONArray = JSONArray().apply {
        sims.forEach {
            put(
                JSONObject()
                    .put("slot", it.slot)
                    .put("carrier", it.carrier)
                    .put("in_service", it.inService)
                    .put("default_sms", it.defaultSms)
            )
        }
    }
}
