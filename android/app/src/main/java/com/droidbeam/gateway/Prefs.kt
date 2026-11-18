package com.droidbeam.gateway

import android.content.Context
import android.content.SharedPreferences
import androidx.core.content.edit

/** Persistent app configuration. The device token is stored in app-private storage. */
class Prefs(context: Context) {
    private val sp: SharedPreferences =
        context.applicationContext.getSharedPreferences("droidbeam", Context.MODE_PRIVATE)

    var serverUrl: String
        get() = sp.getString("server_url", "") ?: ""
        set(v) = sp.edit { putString("server_url", v) }

    /** Optional second address of the same server (e.g. public/cloud URL), used when the primary fails. */
    var fallbackUrl: String
        get() = sp.getString("fallback_url", "") ?: ""
        set(v) = sp.edit { putString("fallback_url", v) }

    /** Ask the server (through the fallback domain) for its LAN address and use it automatically. */
    var autoLan: Boolean
        get() = sp.getBoolean("auto_lan", true)
        set(v) = sp.edit { putBoolean("auto_lan", v) }

    /** Identifies the server so a discovered LAN address can be verified. */
    var serverId: String
        get() = sp.getString("server_id", "") ?: ""
        set(v) = sp.edit { putString("server_id", v) }

    var deviceToken: String
        get() = sp.getString("device_token", "") ?: ""
        set(v) = sp.edit { putString("device_token", v) }

    var deviceId: Long
        get() = sp.getLong("device_id", 0)
        set(v) = sp.edit { putLong("device_id", v) }

    var deviceName: String
        get() = sp.getString("device_name", "") ?: ""
        set(v) = sp.edit { putString("device_name", v) }

    /** Whether the user wants the gateway running (restored after reboot). */
    var enabled: Boolean
        get() = sp.getBoolean("enabled", false)
        set(v) = sp.edit { putBoolean("enabled", v) }

    val isRegistered: Boolean
        get() = (serverUrl.isNotEmpty() || fallbackUrl.isNotEmpty()) && deviceToken.isNotEmpty()

    fun clearRegistration() = sp.edit {
        remove("device_token"); remove("device_id"); remove("device_name"); remove("server_id")
        putBoolean("enabled", false)
    }
}
