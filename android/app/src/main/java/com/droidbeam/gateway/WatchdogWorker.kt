package com.droidbeam.gateway

import android.content.Context
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import java.util.concurrent.TimeUnit

/**
 * Periodically makes sure the gateway service is running while the user has it enabled.
 * Covers cases START_STICKY misses: OEM task killers, force-stops by the system, app updates.
 */
class WatchdogWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        val prefs = Prefs(applicationContext)
        if (!prefs.enabled || !prefs.isRegistered) return Result.success()
        if (!GatewayService.isRunning) {
            GatewayState.log("Watchdog: menyalakan ulang gateway")
            GatewayService.ensureRunning(applicationContext)
        }
        return Result.success()
    }

    companion object {
        private const val PERIODIC = "watchdog"
        private const val ONE_SHOT = "watchdog-now"

        /** 15 minutes is the shortest interval WorkManager allows. */
        fun schedule(context: Context) {
            val request = PeriodicWorkRequestBuilder<WatchdogWorker>(15, TimeUnit.MINUTES).build()
            WorkManager.getInstance(context)
                .enqueueUniquePeriodicWork(PERIODIC, ExistingPeriodicWorkPolicy.KEEP, request)
        }

        /** Re-checks shortly after the service was killed. */
        fun checkSoon(context: Context) {
            val request = OneTimeWorkRequestBuilder<WatchdogWorker>()
                .setInitialDelay(30, TimeUnit.SECONDS)
                .build()
            WorkManager.getInstance(context)
                .enqueueUniqueWork(ONE_SHOT, ExistingWorkPolicy.REPLACE, request)
        }

        fun cancel(context: Context) {
            WorkManager.getInstance(context).apply {
                cancelUniqueWork(PERIODIC)
                cancelUniqueWork(ONE_SHOT)
            }
        }
    }
}
