package com.droidbeam.gateway

import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.BatteryManager
import android.os.Build
import android.telephony.TelephonyManager
import androidx.core.content.ContextCompat
import org.json.JSONObject
import java.net.Inet4Address

/** Collects phone and battery details for the heartbeat. */
object DeviceReporter {

    data class Battery(
        val level: Int,
        val status: String,
        val plugged: String,
        val temperature: Double?,
        val health: String,
    ) {
        /** Fields that matter for webhooks; temperature/voltage changes alone are not worth a report. */
        val key get() = Triple(level, status, plugged)

        fun toJson(): JSONObject = JSONObject()
            .put("level", level)
            .put("status", status)
            .put("plugged", plugged)
            .put("health", health)
            .apply { if (temperature != null) put("temperature", temperature) }

        fun describe(): String {
            val state = when (status) {
                "charging" -> "mengisi"
                "full" -> "penuh"
                "discharging" -> "tidak mengisi"
                "not_charging" -> "tidak mengisi (terhubung)"
                else -> status
            }
            val via = if (plugged != "none") " via ${plugged.uppercase()}" else ""
            val temp = temperature?.let { " · %.1f°C".format(it) } ?: ""
            return "$level% · $state$via$temp"
        }
    }

    /** Reads battery state from an ACTION_BATTERY_CHANGED intent, or the sticky one if null. */
    fun battery(context: Context, intent: Intent? = null): Battery? {
        val i = intent ?: ContextCompat.registerReceiver(
            context, null, IntentFilter(Intent.ACTION_BATTERY_CHANGED), ContextCompat.RECEIVER_NOT_EXPORTED,
        ) ?: return null
        val rawLevel = i.getIntExtra(BatteryManager.EXTRA_LEVEL, -1)
        val scale = i.getIntExtra(BatteryManager.EXTRA_SCALE, 100)
        if (rawLevel < 0 || scale <= 0) return null
        val temp = i.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, Int.MIN_VALUE)
        return Battery(
            level = (rawLevel * 100 / scale).coerceIn(0, 100),
            status = when (i.getIntExtra(BatteryManager.EXTRA_STATUS, -1)) {
                BatteryManager.BATTERY_STATUS_CHARGING -> "charging"
                BatteryManager.BATTERY_STATUS_DISCHARGING -> "discharging"
                BatteryManager.BATTERY_STATUS_FULL -> "full"
                BatteryManager.BATTERY_STATUS_NOT_CHARGING -> "not_charging"
                else -> "unknown"
            },
            plugged = when (i.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0)) {
                BatteryManager.BATTERY_PLUGGED_AC -> "ac"
                BatteryManager.BATTERY_PLUGGED_USB -> "usb"
                BatteryManager.BATTERY_PLUGGED_WIRELESS -> "wireless"
                8 /* BATTERY_PLUGGED_DOCK, API 33 */ -> "dock"
                else -> "none"
            },
            temperature = if (temp == Int.MIN_VALUE) null else temp / 10.0,
            health = when (i.getIntExtra(BatteryManager.EXTRA_HEALTH, -1)) {
                BatteryManager.BATTERY_HEALTH_GOOD -> "good"
                BatteryManager.BATTERY_HEALTH_OVERHEAT -> "overheat"
                BatteryManager.BATTERY_HEALTH_DEAD -> "dead"
                BatteryManager.BATTERY_HEALTH_OVER_VOLTAGE -> "over_voltage"
                BatteryManager.BATTERY_HEALTH_COLD -> "cold"
                BatteryManager.BATTERY_HEALTH_UNSPECIFIED_FAILURE -> "failure"
                else -> "unknown"
            },
        )
    }

    fun info(context: Context, connection: String): JSONObject {
        val tm = context.getSystemService(TelephonyManager::class.java)
        val operator = runCatching { tm?.networkOperatorName?.ifBlank { tm.simOperatorName } }.getOrNull().orEmpty()
        val appVersion = runCatching {
            context.packageManager.getPackageInfo(context.packageName, 0).versionName
        }.getOrNull().orEmpty()
        return JSONObject()
            .put("manufacturer", Build.MANUFACTURER)
            .put("model", Build.MODEL)
            .put("android_version", Build.VERSION.RELEASE)
            .put("sdk", Build.VERSION.SDK_INT)
            .put("app_version", appVersion)
            .put("operator", operator)
            .put("network", networkType(context))
            .put("connection", connection)
            .put("local_ip", localIp(context))
            .put("sims", SimManager.toJson(SimManager.list(context)))
    }

    /** The phone's own IPv4 address on the current network (e.g. 192.168.1.23). */
    private fun localIp(context: Context): String {
        val cm = context.getSystemService(ConnectivityManager::class.java) ?: return ""
        val props = cm.getLinkProperties(cm.activeNetwork) ?: return ""
        return props.linkAddresses
            .map { it.address }
            .firstOrNull { it is Inet4Address && !it.isLoopbackAddress }
            ?.hostAddress.orEmpty()
    }

    private fun networkType(context: Context): String {
        val cm = context.getSystemService(ConnectivityManager::class.java) ?: return "unknown"
        val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return "none"
        return when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "ethernet"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
            else -> "other"
        }
    }
}
