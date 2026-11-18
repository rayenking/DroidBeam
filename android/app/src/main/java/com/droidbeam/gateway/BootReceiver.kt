package com.droidbeam.gateway

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Restarts the gateway after a reboot or an app update if it was running before. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED,
            "android.intent.action.QUICKBOOT_POWERON",      // some Xiaomi/HTC ROMs
            "com.htc.intent.action.QUICKBOOT_POWERON" -> {
                val prefs = Prefs(context)
                if (!prefs.enabled || !prefs.isRegistered) return
                GatewayState.log("Starting gateway after ${if (intent.action == Intent.ACTION_MY_PACKAGE_REPLACED) "app update" else "boot"}")
                WatchdogWorker.schedule(context)
                GatewayService.ensureRunning(context)
            }
        }
    }
}
