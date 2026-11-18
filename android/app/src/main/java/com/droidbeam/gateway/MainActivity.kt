package com.droidbeam.gateway

import android.Manifest
import android.annotation.SuppressLint
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.net.Uri
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class MainActivity : ComponentActivity() {

    private lateinit var prefs: Prefs
    private var permissionsOk by mutableStateOf(false)
    private var registered by mutableStateOf(false)
    private var batteryExempt by mutableStateOf(false)

    private val requiredPermissions: Array<String> = buildList {
        add(Manifest.permission.SEND_SMS)
        add(Manifest.permission.RECEIVE_SMS)
        add(Manifest.permission.READ_PHONE_STATE)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) add(Manifest.permission.POST_NOTIFICATIONS)
    }.toTypedArray()

    private val permissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { refresh() }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        enableEdgeToEdge()
        setContent {
            val dark = isSystemInDarkTheme()
            MaterialTheme(colorScheme = if (dark) darkColorScheme() else lightColorScheme(primary = Color(0xFF0F766E))) {
                MainScreen()
            }
        }
    }

    override fun onResume() {
        super.onResume()
        refresh()
    }

    private fun refresh() {
        permissionsOk = requiredPermissions.all {
            ContextCompat.checkSelfPermission(this, it) == PackageManager.PERMISSION_GRANTED
        }
        registered = prefs.isRegistered
        batteryExempt = getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)
        // Opening the app is a good moment to bring a killed gateway back.
        if (prefs.enabled && prefs.isRegistered && !GatewayService.isRunning) GatewayService.ensureRunning(this)
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun MainScreen() {
        val state by GatewayState.state.collectAsStateWithLifecycle()
        Scaffold(topBar = { TopAppBar(title = { Text("DroidBeam") }) }) { padding ->
            Column(
                Modifier
                    .fillMaxSize()
                    .padding(padding)
                    .verticalScroll(rememberScrollState())
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                if (registered) RegisteredCard(state.running) else RegisterCard()
                PermissionCard()
                if (registered) GatewayCard(state)
                LogCard(state.log)
            }
        }
    }

    @Composable
    private fun RegisterCard() {
        var url by remember { mutableStateOf(prefs.serverUrl) }
        var fallback by remember { mutableStateOf(prefs.fallbackUrl) }
        var autoLan by remember { mutableStateOf(prefs.autoLan) }
        var apiKey by remember { mutableStateOf("") }
        var name by remember { mutableStateOf(Build.MODEL) }
        var busy by remember { mutableStateOf(false) }
        var error by remember { mutableStateOf<String?>(null) }
        val scope = rememberCoroutineScope()

        Section("Connect to server") {
            OutlinedTextField(
                url, { url = it }, Modifier.fillMaxWidth(),
                label = { Text("LAN address (optional with auto-discovery)") }, singleLine = true,
                placeholder = { Text("http://192.168.1.10:8080") },
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            )
            OutlinedTextField(
                fallback, { fallback = it }, Modifier.fillMaxWidth(),
                label = { Text("Cloud / domain address") }, singleLine = true,
                placeholder = { Text("https://sms.domainmu.com") },
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            )
            AutoLanSwitch(autoLan) { autoLan = it }
            OutlinedTextField(
                apiKey, { apiKey = it }, Modifier.fillMaxWidth(),
                label = { Text("Server API key") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(),
            )
            OutlinedTextField(
                name, { name = it }, Modifier.fillMaxWidth(),
                label = { Text("Device name") }, singleLine = true,
            )
            error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            Button(
                enabled = !busy && (url.length > 8 || fallback.length > 8) && apiKey.isNotBlank() && name.isNotBlank(),
                onClick = {
                    busy = true
                    error = null
                    scope.launch {
                        val result = withContext(Dispatchers.IO) {
                            runCatching {
                                val reg = ApiClient({ url }, { fallback }).register(apiKey.trim(), name.trim())
                                // Remember the server's id so discovered LAN addresses can be verified.
                                val serverId = runCatching {
                                    ApiClient({ url }, { fallback }, reg.token).serverInfo().serverId
                                }.getOrDefault("")
                                reg to serverId
                            }
                        }
                        busy = false
                        result.onSuccess { (reg, serverId) ->
                            prefs.serverId = serverId
                            prefs.autoLan = autoLan
                            prefs.serverUrl = url.trim().trimEnd('/')
                            prefs.fallbackUrl = fallback.trim().trimEnd('/')
                            prefs.deviceToken = reg.token
                            prefs.deviceId = reg.deviceId
                            prefs.deviceName = reg.name
                            GatewayState.log("Registered as device #${reg.deviceId}")
                            refresh()
                        }.onFailure { error = "Registration failed: ${it.message ?: it.javaClass.simpleName}" }
                    }
                },
            ) { Text(if (busy) "Registering…" else "Register device") }
        }
    }

    @Composable
    private fun RegisteredCard(running: Boolean) {
        var url by remember { mutableStateOf(prefs.serverUrl) }
        var fallback by remember { mutableStateOf(prefs.fallbackUrl) }
        var autoLan by remember { mutableStateOf(prefs.autoLan) }
        // The LAN address may be updated in the background by auto-discovery.
        val state by GatewayState.state.collectAsStateWithLifecycle()
        val discovered = state.endpointUrl
        if (!running && discovered != null && prefs.serverUrl == discovered && url != discovered && url.isBlank()) url = discovered
        val changed = url.trim().trimEnd('/') != prefs.serverUrl ||
            fallback.trim().trimEnd('/') != prefs.fallbackUrl || autoLan != prefs.autoLan

        Section("Server") {
            Text(
                "Device #${prefs.deviceId} · ${prefs.deviceName}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedTextField(
                url, { url = it }, Modifier.fillMaxWidth(),
                label = { Text("LAN address") }, singleLine = true, enabled = !running,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            )
            OutlinedTextField(
                fallback, { fallback = it }, Modifier.fillMaxWidth(),
                label = { Text("Cloud / domain address") }, singleLine = true, enabled = !running,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            )
            AutoLanSwitch(autoLan, enabled = !running) { autoLan = it }
            if (running) {
                Text(
                    "Stop the gateway to change addresses.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = !running && changed && (url.length > 8 || fallback.length > 8),
                    onClick = {
                        prefs.serverUrl = url.trim().trimEnd('/')
                        prefs.fallbackUrl = fallback.trim().trimEnd('/')
                        prefs.autoLan = autoLan
                        GatewayState.log("Server addresses updated")
                    },
                ) { Text("Save addresses") }
                TextButton(
                    enabled = !running,
                    onClick = { prefs.clearRegistration(); refresh() },
                ) { Text("Unregister device") }
            }
        }
    }

    @Composable
    private fun AutoLanSwitch(checked: Boolean, enabled: Boolean = true, onChange: (Boolean) -> Unit) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Discover LAN address via domain", style = MaterialTheme.typography.bodyMedium)
                Text(
                    "The server reports its LAN address; the app switches to LAN when reachable.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Switch(checked = checked, onCheckedChange = onChange, enabled = enabled)
        }
    }

    @Composable
    private fun PermissionCard() {
        Section("Permissions & battery") {
            if (permissionsOk) {
                Text("✓ SMS, SIM and notification permissions granted")
            } else {
                Text("The app needs permission to send/receive SMS, read SIM status and show notifications.")
                Button(onClick = { permissionLauncher.launch(requiredPermissions) }) { Text("Grant permissions") }
            }
            if (batteryExempt) {
                Text("✓ DroidBeam is exempt from battery optimization")
            } else {
                Text(
                    "So the system doesn't kill the gateway and it can restart on its own, " +
                        "exempt DroidBeam from battery optimization.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Button(onClick = { requestBatteryExemption() }) { Text("Exempt from battery optimization") }
            }
            Text(
                "Xiaomi/Oppo/Vivo/Realme/Samsung: also enable \"Autostart\" and set battery to \"No restrictions\" in the app settings.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedButton(onClick = {
                startActivity(
                    Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")),
                )
            }) { Text("Open app settings") }
        }
    }

    @SuppressLint("BatteryLife") // a gateway must keep running; the app is sideloaded, not on Play
    private fun requestBatteryExemption() {
        val direct = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName"))
        runCatching { startActivity(direct) }.onFailure {
            startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
        }
    }

    @Composable
    private fun GatewayCard(state: GatewayState.Snapshot) {
        Section("Gateway") {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(if (state.running) "Running" else "Stopped", style = MaterialTheme.typography.titleMedium)
                    val status = when {
                        !state.running -> "Turn on to start sending and forwarding SMS"
                        state.connected -> "Connected to server"
                        state.lastError != null -> state.lastError
                        else -> "Connecting…"
                    }
                    Text(status, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                Spacer(Modifier.width(12.dp))
                Switch(
                    checked = state.running,
                    enabled = permissionsOk,
                    onCheckedChange = { on ->
                        if (on) GatewayService.start(this@MainActivity) else GatewayService.stop(this@MainActivity)
                    },
                )
            }
            if (state.running && state.connected) {
                Text(
                    "Connection: ${state.endpointLabel ?: "-"}${state.endpointUrl?.let { " · $it" } ?: ""}",
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            Text(
                "Battery: ${state.battery ?: DeviceReporter.battery(this@MainActivity)?.describe() ?: "-"}",
                style = MaterialTheme.typography.bodySmall,
            )
            SimManager.list(this@MainActivity).forEach {
                Text(it.describe(), style = MaterialTheme.typography.bodySmall)
            }
            Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                Stat("Sent", state.sent)
                Stat("Failed", state.failed)
                Stat("Received", state.received)
            }
        }
    }

    @Composable
    private fun Stat(label: String, value: Int) {
        Column {
            Text(value.toString(), style = MaterialTheme.typography.titleLarge)
            Text(label, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }

    @Composable
    private fun LogCard(lines: List<String>) {
        Section("Activity") {
            if (lines.isEmpty()) {
                Text("No activity yet", color = MaterialTheme.colorScheme.onSurfaceVariant)
            } else {
                lines.take(30).forEach {
                    Text(it, fontFamily = FontFamily.Monospace, fontSize = 12.sp)
                }
            }
        }
    }

    @Composable
    private fun Section(title: String, content: @Composable () -> Unit) {
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(title, style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.primary)
                content()
            }
        }
    }
}
