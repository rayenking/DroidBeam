package com.droidbeam.gateway

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager

class App : Application() {
    override fun onCreate() {
        super.onCreate()
        val channel = NotificationChannel(
            CHANNEL_GATEWAY,
            "Status gateway",
            NotificationManager.IMPORTANCE_LOW,
        ).apply { description = "Notifikasi tetap saat gateway SMS berjalan" }
        getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
    }

    companion object {
        const val CHANNEL_GATEWAY = "gateway"
    }
}
