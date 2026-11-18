package com.droidbeam.gateway

import android.annotation.SuppressLint
import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.wifi.WifiManager
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import okhttp3.OkHttpClient
import java.io.IOException
import kotlin.coroutines.coroutineContext

/**
 * Foreground service that long-polls the server for queued messages and sends them.
 *
 * Stability measures:
 *  - START_STICKY + BootReceiver + [WatchdogWorker] bring it back if the system kills it.
 *  - A partial wake lock and Wi-Fi lock keep the long-poll alive with the screen off.
 *  - A network callback cuts the reconnect backoff short as soon as connectivity returns.
 *  - Requests fail over from the primary (LAN) address to the fallback (cloud) and back.
 *  - Every error inside the loop is caught; the loop itself is restarted if it ever exits.
 */
class GatewayService : Service() {

    private val crashGuard = CoroutineExceptionHandler { _, e -> Log.e(TAG, "unexpected error", e) }
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO + crashGuard)
    private var pollJob: Job? = null
    // Own client so onDestroy can abort the in-flight long-poll without touching other calls.
    private val http: OkHttpClient = ApiClient.sharedClient.newBuilder().build()
    private val networkBack = Channel<Unit>(Channel.CONFLATED)
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiLock: WifiManager.WifiLock? = null
    private var lastNotificationText: String? = null
    private var heartbeatJob: Job? = null
    private val batteryChanged = Channel<DeviceReporter.Battery>(Channel.CONFLATED)
    private var batteryReceiver: BroadcastReceiver? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        isRunning = true
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // Must call startForeground promptly on every start, even when about to stop.
        startInForeground()
        if (intent?.action == ACTION_STOP) {
            Prefs(this).enabled = false
            stopSelf()
            return START_NOT_STICKY
        }
        val prefs = Prefs(this)
        if (!prefs.isRegistered || !prefs.enabled) {
            if (!prefs.isRegistered) GatewayState.error("Perangkat belum terdaftar")
            stopSelf()
            return START_NOT_STICKY
        }
        acquireLocks()
        registerNetworkCallback()
        WatchdogWorker.schedule(this)
        if (pollJob?.isActive != true) {
            GatewayState.setRunning(true)
            GatewayState.log(if (intent == null) "Gateway dipulihkan oleh sistem" else "Gateway dimulai")
            pollJob = scope.launch { superviseLoop(ApiClient.forPrefs(prefs, http)) }
        }
        registerBatteryReceiver()
        if (heartbeatJob?.isActive != true) {
            heartbeatJob = scope.launch { heartbeatLoop(ApiClient.forPrefs(prefs, http)) }
        }
        return START_STICKY
    }

    /** Restarts the poll loop if it ever ends unexpectedly. */
    private suspend fun superviseLoop(api: ApiClient) {
        while (coroutineContext.isActive) {
            try {
                if (!pollLoop(api)) return // fatal (token revoked): stop for good
            } catch (e: CancellationException) {
                throw e
            } catch (e: Throwable) {
                Log.e(TAG, "poll loop crashed", e)
                GatewayState.error("Kesalahan internal: ${e.javaClass.simpleName}")
                delay(5_000)
            }
        }
    }

    /** Returns false when the gateway must stop (e.g. the device token was revoked). */
    private suspend fun pollLoop(api: ApiClient): Boolean {
        var backoffMs = MIN_BACKOFF_MS
        while (coroutineContext.isActive) {
            try {
                api.maybeRestorePrimary()
                val messages = api.fetchOutbox(POLL_WAIT_SECONDS)
                GatewayState.connected()
                updateNotification("Terhubung (${if (api.connection == "primary") "LAN" else "cloud"}) · menunggu SMS")
                backoffMs = MIN_BACKOFF_MS
                for (msg in messages) {
                    sendOne(msg)
                    delay(SEND_INTERVAL_MS)
                }
                continue
            } catch (e: CancellationException) {
                throw e
            } catch (e: ApiException) {
                if (e.code == 401) {
                    GatewayState.error("Token perangkat ditolak server. Daftarkan ulang perangkat.")
                    Prefs(this).enabled = false
                    stopSelf()
                    return false
                }
                GatewayState.error("Server: ${e.message}")
            } catch (e: IOException) {
                if (!coroutineContext.isActive) break
                GatewayState.error("Tidak bisa terhubung: ${e.message ?: e.javaClass.simpleName}")
            } catch (e: Exception) {
                // Malformed response etc. Never let it kill the service.
                Log.e(TAG, "poll error", e)
                GatewayState.error("Respons server tidak valid: ${e.javaClass.simpleName}")
            }
            updateNotification("Terputus · mencoba lagi dalam ${backoffMs / 1000} dtk")
            networkBack.tryReceive() // discard stale signal
            withTimeoutOrNull(backoffMs) { networkBack.receive() } // wake early when network returns
            backoffMs = (backoffMs * 2).coerceAtMost(MAX_BACKOFF_MS)
        }
        return true
    }

    private fun sendOne(msg: OutboxMessage) {
        try {
            val slot = SmsSender.send(this, msg)
            GatewayState.log("#${msg.id} mengirim ke ${msg.to}${slot?.let { " lewat SIM $it" } ?: ""}")
        } catch (e: Exception) {
            // e.g. SecurityException when SEND_SMS was revoked, or an invalid number.
            val reason = e.message ?: e.javaClass.simpleName
            ReportWorker.enqueueStatus(this, msg.id, "failed", reason)
            GatewayState.countFailed()
            GatewayState.log("#${msg.id} gagal: $reason")
        }
    }

    // ---- heartbeat (device info + battery) ----

    /** Reports device info and battery on every meaningful battery change and every few minutes. */
    private suspend fun heartbeatLoop(api: ApiClient) {
        var lastSentKey: Any? = null
        var lastSentAt = 0L
        var pending: DeviceReporter.Battery? = DeviceReporter.battery(this)
        while (coroutineContext.isActive) {
            val battery = pending ?: DeviceReporter.battery(this)
            pending = null
            battery?.let { GatewayState.setBattery(it.describe()) }
            val due = System.currentTimeMillis() - lastSentAt >= HEARTBEAT_INTERVAL_MS
            if (due || battery?.key != lastSentKey) {
                try {
                    api.heartbeat(DeviceReporter.info(this, api.connection), battery?.toJson())
                    lastSentKey = battery?.key
                    lastSentAt = System.currentTimeMillis()
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    // The poll loop reports connectivity problems; just retry sooner.
                    Log.w(TAG, "heartbeat failed", e)
                    pending = withTimeoutOrNull(HEARTBEAT_RETRY_MS) { batteryChanged.receive() }
                    continue
                }
            }
            pending = withTimeoutOrNull(HEARTBEAT_INTERVAL_MS) { batteryChanged.receive() }
        }
    }

    private fun registerBatteryReceiver() {
        if (batteryReceiver != null) return
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) {
                DeviceReporter.battery(context, intent)?.let { batteryChanged.trySend(it) }
            }
        }
        ContextCompat.registerReceiver(
            this, receiver, IntentFilter(Intent.ACTION_BATTERY_CHANGED), ContextCompat.RECEIVER_NOT_EXPORTED,
        )
        batteryReceiver = receiver
    }

    // ---- locks & network ----

    @SuppressLint("WakelockTimeout") // held for the lifetime of the service by design
    private fun acquireLocks() {
        if (wakeLock == null) {
            wakeLock = getSystemService(PowerManager::class.java)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "DroidBeam::gateway")
                .apply { setReferenceCounted(false); acquire() }
        }
        if (wifiLock == null) {
            val mode = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                WifiManager.WIFI_MODE_FULL_LOW_LATENCY
            } else {
                @Suppress("DEPRECATION") WifiManager.WIFI_MODE_FULL_HIGH_PERF
            }
            wifiLock = runCatching {
                applicationContext.getSystemService(WifiManager::class.java)
                    .createWifiLock(mode, "DroidBeam::gateway")
                    .apply { setReferenceCounted(false); acquire() }
            }.getOrNull()
        }
    }

    private fun releaseLocks() {
        runCatching { wakeLock?.takeIf { it.isHeld }?.release() }
        runCatching { wifiLock?.takeIf { it.isHeld }?.release() }
        wakeLock = null
        wifiLock = null
    }

    private fun registerNetworkCallback() {
        if (networkCallback != null) return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                ApiClient.probePrimarySoon() // e.g. back on office Wi-Fi: try LAN again
                networkBack.trySend(Unit)
            }
        }
        runCatching {
            getSystemService(ConnectivityManager::class.java).registerDefaultNetworkCallback(cb)
            networkCallback = cb
        }
    }

    private fun unregisterNetworkCallback() {
        networkCallback?.let { cb ->
            runCatching { getSystemService(ConnectivityManager::class.java).unregisterNetworkCallback(cb) }
        }
        networkCallback = null
    }

    // ---- notification ----

    private fun startInForeground() {
        val notification = buildNotification(lastNotificationText ?: "Menghubungkan ke server…")
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    private fun updateNotification(text: String) {
        if (text == lastNotificationText) return
        lastNotificationText = text
        runCatching {
            getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, buildNotification(text))
        }
    }

    private fun buildNotification(text: String): Notification {
        val openApp = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE,
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, GatewayService::class.java).setAction(ACTION_STOP), PendingIntent.FLAG_IMMUTABLE,
        )
        return NotificationCompat.Builder(this, App.CHANNEL_GATEWAY)
            .setSmallIcon(R.drawable.ic_stat_beam)
            .setContentTitle("DroidBeam aktif")
            .setContentText(text)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .setContentIntent(openApp)
            .addAction(0, "Hentikan", stop)
            .build()
    }

    // ---- lifecycle ----

    override fun onTaskRemoved(rootIntent: Intent?) {
        // Some OEM ROMs kill the service when the app is swiped away; ask the watchdog to check soon.
        if (Prefs(this).enabled) WatchdogWorker.checkSoon(this)
        super.onTaskRemoved(rootIntent)
    }

    override fun onDestroy() {
        isRunning = false
        scope.cancel()
        http.dispatcher.cancelAll()
        unregisterNetworkCallback()
        batteryReceiver?.let { runCatching { unregisterReceiver(it) } }
        batteryReceiver = null
        releaseLocks()
        GatewayState.setRunning(false)
        GatewayState.log("Gateway berhenti")
        // Killed while the user still wants it on (low memory, OEM task killer): schedule recovery.
        if (Prefs(this).enabled) WatchdogWorker.checkSoon(this)
        super.onDestroy()
    }

    companion object {
        private const val TAG = "GatewayService"
        private const val ACTION_STOP = "com.droidbeam.gateway.STOP"
        private const val NOTIFICATION_ID = 1
        private const val POLL_WAIT_SECONDS = 25
        /** Pause between messages so the carrier does not flag the SIM for spam. */
        private const val SEND_INTERVAL_MS = 1500L
        private const val MIN_BACKOFF_MS = 2_000L
        private const val MAX_BACKOFF_MS = 60_000L
        private const val HEARTBEAT_INTERVAL_MS = 5 * 60_000L
        private const val HEARTBEAT_RETRY_MS = 30_000L

        @Volatile
        var isRunning = false
            private set

        /** Starts the gateway and remembers the choice so it comes back after reboot. */
        fun start(context: Context) {
            Prefs(context).enabled = true
            WatchdogWorker.schedule(context)
            ensureRunning(context)
        }

        fun stop(context: Context) {
            Prefs(context).enabled = false
            WatchdogWorker.cancel(context)
            context.stopService(Intent(context, GatewayService::class.java))
        }

        /**
         * Starts the service if the user enabled it. Returns false if Android refused
         * (background-start restrictions on Android 12+ without a battery-optimisation exemption).
         */
        fun ensureRunning(context: Context): Boolean {
            val prefs = Prefs(context)
            if (!prefs.enabled || !prefs.isRegistered) return true
            return try {
                ContextCompat.startForegroundService(context, Intent(context, GatewayService::class.java))
                true
            } catch (e: Exception) {
                // ForegroundServiceStartNotAllowedException / IllegalStateException
                Log.w(TAG, "could not start gateway", e)
                GatewayState.error("Android menolak menjalankan gateway di latar belakang. Nonaktifkan optimasi baterai.")
                false
            }
        }
    }
}
