package com.droidbeam.gateway

import android.util.Log
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.io.IOException
import java.util.concurrent.TimeUnit

data class OutboxMessage(val id: Long, val to: String, val body: String, val sim: Int? = null)

data class Registration(val deviceId: Long, val name: String, val token: String)

data class ServerInfo(val serverId: String, val lanUrls: List<String>)

/** Non-2xx response from the server. */
class ApiException(val code: Int, message: String) : IOException(message) {
    val isClientError get() = code in 400..499 && code != 408 && code != 429
}

/**
 * Client for the DroidBeam server, which is reachable through two addresses of the same server:
 *  - primary: usually the LAN address (fast, works without internet)
 *  - fallback: usually the public/cloud domain
 *
 * On a connection failure a request is retried on the other address. The address that worked is
 * remembered process-wide, and [maybeRestorePrimary] moves back to the primary when possible.
 * If [onLanDiscovered] is set, the client also asks the server (through the fallback) for its
 * current LAN address, so the LAN IP does not have to be typed in or kept up to date by hand.
 * HTTP errors (4xx/5xx) never trigger failover: the server was reached.
 */
class ApiClient(
    private val primary: () -> String,
    private val fallback: () -> String,
    private val deviceToken: String? = null,
    private val client: OkHttpClient = sharedClient,
    private val expectedServerId: () -> String = { "" },
    private val onLanDiscovered: ((url: String, serverId: String) -> Unit)? = null,
) {
    private fun slot(i: Int): String = (if (i == 0) primary() else fallback()).trim().trimEnd('/')

    // ---- client API ----

    fun register(apiKey: String, name: String): Registration = withFailover { base ->
        val json = execute(
            Request.Builder()
                .url("$base/api/v1/devices")
                .header("X-API-Key", apiKey)
                .post(JSONObject().put("name", name).toBody())
                .build()
        )
        val device = json.getJSONObject("device")
        Registration(device.getLong("id"), device.getString("name"), json.getString("token"))
    }

    // ---- device API ----

    /** Long-polls for messages to send. Returns an empty list on timeout. */
    fun fetchOutbox(waitSeconds: Int): List<OutboxMessage> = withFailover { base ->
        val json = execute(device("$base/api/v1/device/outbox?wait=$waitSeconds").get().build())
        val data = json.getJSONArray("data")
        List(data.length()) { i ->
            val m = data.getJSONObject(i)
            OutboxMessage(
                m.getLong("id"), m.getString("to"), m.getString("body"),
                if (m.isNull("sim")) null else m.optInt("sim"),
            )
        }
    }

    fun reportStatus(messageId: Long, status: String, error: String?, sim: Int?) = withFailover { base ->
        val body = JSONObject().put("status", status)
        if (error != null) body.put("error", error)
        if (sim != null) body.put("sim", sim)
        execute(device("$base/api/v1/device/messages/$messageId/status").post(body.toBody()).build())
        Unit
    }

    fun reportIncoming(uid: String, from: String, body: String, receivedAt: Long, sim: Int?) = withFailover { base ->
        val json = JSONObject()
            .put("uid", uid)
            .put("from", from)
            .put("body", body)
            .put("received_at", receivedAt)
        if (sim != null) json.put("sim", sim)
        execute(device("$base/api/v1/device/inbox").post(json.toBody()).build())
        Unit
    }

    fun heartbeat(info: JSONObject, battery: JSONObject?) = withFailover { base ->
        val json = JSONObject().put("info", info)
        if (battery != null) json.put("battery", battery)
        execute(device("$base/api/v1/device/heartbeat").post(json.toBody()).build())
        Unit
    }

    fun serverInfo(): ServerInfo = withFailover { base -> fetchServerInfo(base) }

    private fun fetchServerInfo(base: String): ServerInfo {
        val json = execute(device("$base/api/v1/device/server-info").get().build())
        val urls = json.optJSONArray("lan_urls")
        return ServerInfo(
            json.optString("server_id"),
            List(urls?.length() ?: 0) { urls!!.getString(it).trim().trimEnd('/') },
        )
    }

    // ---- failover ----

    /** "primary" or "fallback": which address is currently in use. */
    val connection: String get() = if (activeIndex == 0) "primary" else "fallback"

    /**
     * While on the fallback (or with no primary yet), periodically try to get back to the LAN:
     * first the saved primary address, then any LAN address the server reports via the fallback.
     */
    fun maybeRestorePrimary() {
        val primaryUrl = slot(0)
        val fallbackUrl = slot(1)
        if (activeIndex == 0 && primaryUrl.isNotEmpty()) return
        if (fallbackUrl.isEmpty()) return
        val now = System.currentTimeMillis()
        if (now - lastPrimaryProbe < PRIMARY_PROBE_INTERVAL_MS) return
        lastPrimaryProbe = now

        if (primaryUrl.isNotEmpty() && probe(primaryUrl, expectedServerId())) {
            switchTo(0)
            return
        }
        val discover = onLanDiscovered ?: return
        val info = runCatching { fetchServerInfo(fallbackUrl) }.getOrElse {
            Log.w(TAG, "LAN discovery failed: ${it.message}")
            return
        }
        for (candidate in info.lanUrls) {
            if (candidate == primaryUrl) continue
            if (probe(candidate, info.serverId)) {
                discover(candidate, info.serverId)
                GatewayState.log("IP LAN server ditemukan lewat domain: $candidate")
                switchTo(0)
                return
            }
        }
    }

    /** Healthy and, when the server id is known, really the same server (not another LAN host). */
    private fun probe(base: String, serverId: String): Boolean = runCatching {
        probeClient.newCall(Request.Builder().url("$base/healthz").build()).execute().use { resp ->
            if (!resp.isSuccessful) return@use false
            if (serverId.isEmpty()) return@use true
            JSONObject(resp.body?.string().orEmpty()).optString("server_id") == serverId
        }
    }.getOrDefault(false)

    private fun <T> withFailover(block: (base: String) -> T): T {
        val order = if (activeIndex == 0) listOf(0, 1) else listOf(1, 0)
        var lastError: IOException? = null
        for (i in order) {
            val base = slot(i)
            if (base.isEmpty()) continue
            try {
                val result = block(base)
                if (i != activeIndex) switchTo(i)
                return result
            } catch (e: ApiException) {
                throw e // reached the server; the other address would get the same answer
            } catch (e: IOException) {
                if (e.message == "Canceled") throw e // cancelled on purpose (service stopping)
                Log.w(TAG, "$base unreachable: ${e.message}")
                lastError = e
            }
        }
        throw lastError ?: IOException("Alamat server belum diisi")
    }

    private fun switchTo(i: Int) {
        val changed = i != activeIndex
        activeIndex = i
        if (i != 0) lastPrimaryProbe = System.currentTimeMillis()
        GatewayState.setEndpoint(endpointLabel(i), slot(i))
        if (changed) GatewayState.log("Beralih ke alamat ${endpointLabel(i)}: ${slot(i)}")
    }

    private fun device(url: String): Request.Builder {
        val token = requireNotNull(deviceToken) { "device token required" }
        return Request.Builder().url(url).header("Authorization", "Bearer $token")
    }

    private fun execute(request: Request): JSONObject {
        client.newCall(request).execute().use { resp ->
            val text = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) {
                val msg = runCatching { JSONObject(text).getString("error") }.getOrNull() ?: "HTTP ${resp.code}"
                throw ApiException(resp.code, msg)
            }
            return if (text.isBlank()) JSONObject() else JSONObject(text)
        }
    }

    private fun JSONObject.toBody() = toString().toRequestBody(JSON)

    companion object {
        private const val TAG = "ApiClient"
        private val JSON = "application/json".toMediaType()
        private const val PRIMARY_PROBE_INTERVAL_MS = 60_000L

        /** Index of the address in use (0 = primary, 1 = fallback), shared by the service and workers. */
        @Volatile
        private var activeIndex = 0

        @Volatile
        private var lastPrimaryProbe = 0L

        fun endpointLabel(i: Int) = if (i == 0) "LAN" else "cloud"

        /** Forces a LAN probe on the next [maybeRestorePrimary], e.g. after a network change. */
        fun probePrimarySoon() {
            lastPrimaryProbe = 0
        }

        fun forPrefs(prefs: Prefs, client: OkHttpClient = sharedClient) = ApiClient(
            primary = { prefs.serverUrl },
            fallback = { prefs.fallbackUrl },
            deviceToken = prefs.deviceToken,
            client = client,
            expectedServerId = { prefs.serverId },
            onLanDiscovered = { url: String, id: String ->
                if (prefs.autoLan) {
                    prefs.serverUrl = url
                    if (id.isNotEmpty()) prefs.serverId = id
                }
            }.takeIf { prefs.autoLan },
        )

        /** Read timeout must exceed the server's long-poll wait. */
        val sharedClient: OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(8, TimeUnit.SECONDS)
            .readTimeout(60, TimeUnit.SECONDS)
            .writeTimeout(15, TimeUnit.SECONDS)
            .callTimeout(90, TimeUnit.SECONDS)
            .retryOnConnectionFailure(true)
            .build()

        private val probeClient: OkHttpClient = sharedClient.newBuilder()
            .connectTimeout(3, TimeUnit.SECONDS)
            .readTimeout(3, TimeUnit.SECONDS)
            .callTimeout(5, TimeUnit.SECONDS)
            .build()
    }
}
