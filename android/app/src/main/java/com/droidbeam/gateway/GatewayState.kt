package com.droidbeam.gateway

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/** In-memory runtime state shown on the main screen. */
object GatewayState {
    data class Snapshot(
        val running: Boolean = false,
        val connected: Boolean = false,
        val lastError: String? = null,
        val endpointLabel: String? = null,
        val endpointUrl: String? = null,
        val battery: String? = null,
        val sent: Int = 0,
        val failed: Int = 0,
        val received: Int = 0,
        val log: List<String> = emptyList(),
    )

    private val _state = MutableStateFlow(Snapshot())
    val state: StateFlow<Snapshot> = _state

    private val timeFmt = SimpleDateFormat("HH:mm:ss", Locale.US)

    fun setRunning(running: Boolean) = _state.update {
        if (running) it.copy(running = true) else it.copy(running = false, connected = false)
    }

    fun connected() = _state.update {
        if (it.connected && it.lastError == null) it else it.copy(connected = true, lastError = null)
    }

    fun error(message: String) {
        _state.update { it.copy(connected = false, lastError = message) }
        log("⚠ $message")
    }

    fun setEndpoint(label: String, url: String) = _state.update { it.copy(endpointLabel = label, endpointUrl = url) }
    fun setBattery(text: String) = _state.update { it.copy(battery = text) }

    fun countSent() = _state.update { it.copy(sent = it.sent + 1) }
    fun countFailed() = _state.update { it.copy(failed = it.failed + 1) }
    fun countReceived() = _state.update { it.copy(received = it.received + 1) }

    fun log(line: String) {
        val entry = "${synchronized(timeFmt) { timeFmt.format(Date()) }}  $line"
        _state.update { it.copy(log = (listOf(entry) + it.log).take(100)) }
    }
}
