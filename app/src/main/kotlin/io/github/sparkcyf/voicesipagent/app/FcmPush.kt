package io.github.sparkcyf.voicesipagent.app

import android.content.Context
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.google.firebase.messaging.FirebaseMessaging
import java.net.HttpURLConnection
import java.net.URL
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicBoolean

/** Short lived FCM setup and HTTPS token registration; it owns no long-running connection. */
internal object FcmPush {
    private val _status = MutableStateFlow("Not configured")
    val status: StateFlow<String> = _status
    private val registering = AtomicBoolean(false)

    fun refresh(context: Context, settings: GatewaySettings) {
        if (!settings.onDemandSip) {
            _status.value = "Disabled"
            return
        }
        if (!configured()) {
            _status.value = "Firebase project configuration missing"
            return
        }
        if (!settings.wakeServerUrl.startsWith("https://") || settings.wakeRegistrationKey.isBlank()) {
            _status.value = "Wake server setup incomplete"
            return
        }
        if (!registering.compareAndSet(false, true)) return
        _status.value = "Getting FCM token"
        try {
            val app = FirebaseApp.getApps(context).firstOrNull() ?: FirebaseApp.initializeApp(context,
                FirebaseOptions.Builder()
                    .setApplicationId(BuildConfig.FIREBASE_APP_ID)
                    .setApiKey(BuildConfig.FIREBASE_API_KEY)
                    .setProjectId(BuildConfig.FIREBASE_PROJECT_ID)
                    .setGcmSenderId(BuildConfig.FIREBASE_SENDER_ID)
                    .build()) ?: error("Firebase initialization failed")
            FirebaseMessaging.getInstance(app).token.addOnCompleteListener { task ->
                if (!task.isSuccessful) {
                    registering.set(false)
                    _status.value = "FCM token unavailable"
                    AppLog.w("FCM token request failed", task.exception)
                    return@addOnCompleteListener
                }
                val token = task.result
                Thread({
                    try {
                        registerToken(context.applicationContext, settings, token)
                        _status.value = "Registered"
                    } catch (e: Exception) {
                        _status.value = "Registration failed (${e.javaClass.simpleName})"
                        AppLog.w("FCM token registration failed", e)
                    } finally {
                        registering.set(false)
                    }
                }, "fcm-token-register").apply { isDaemon = true; start() }
            }
        } catch (e: Exception) {
            registering.set(false)
            _status.value = "FCM initialization failed (${e.javaClass.simpleName})"
            AppLog.w("FCM initialization failed", e)
        }
    }

    fun reportReady(context: Context, settings: GatewaySettings, requestId: String, onResult: (Boolean) -> Unit) {
        if (!requestId.matches(Regex("[A-Za-z0-9_-]{16,96}"))) {
            onResult(false)
            return
        }
        Thread({
            val ready = runCatching {
                val endpoint = URL(settings.wakeServerUrl.trimEnd('/') + "/api/v1/wake/$requestId/ready")
                require(endpoint.protocol == "https" && endpoint.userInfo == null && endpoint.query == null && endpoint.ref == null)
                val connection = endpoint.openConnection() as javax.net.ssl.HttpsURLConnection
                try {
                    connection.instanceFollowRedirects = false
                    connection.connectTimeout = 5_000
                    connection.readTimeout = 5_000
                    connection.requestMethod = "POST"
                    connection.doOutput = true
                    connection.setRequestProperty("Authorization", "Bearer ${settings.wakeRegistrationKey}")
                    connection.setRequestProperty("Content-Type", "application/json")
                    val payload = JSONObject().put("installation_id", SettingsStore(context).deviceId())
                        .put("sip_registered", true).put("cellular_ready", true).put("shizuku_ready", true)
                        .toString().toByteArray(Charsets.UTF_8)
                    connection.outputStream.use { it.write(payload) }
                    connection.responseCode == HttpURLConnection.HTTP_NO_CONTENT
                } finally {
                    connection.disconnect()
                }
            }.getOrDefault(false)
            if (!ready) AppLog.w("wake readiness confirmation failed")
            onResult(ready)
        }, "fcm-readiness-report").apply { isDaemon = true; start() }
    }

    private fun configured() = BuildConfig.FIREBASE_API_KEY.isNotBlank() &&
        BuildConfig.FIREBASE_APP_ID.isNotBlank() && BuildConfig.FIREBASE_PROJECT_ID.isNotBlank() &&
        BuildConfig.FIREBASE_SENDER_ID.isNotBlank()

    private fun registerToken(context: Context, settings: GatewaySettings, token: String) {
        val base = settings.wakeServerUrl.trimEnd('/')
        val endpoint = URL("$base/api/v1/devices/token")
        require(endpoint.userInfo == null && endpoint.query == null && endpoint.ref == null) {
            "wake server URL must not contain user info, a query, or a fragment"
        }
        val connection = endpoint.openConnection() as HttpURLConnection
        try {
            require(connection is javax.net.ssl.HttpsURLConnection) { "TLS is required for token registration" }
            connection.instanceFollowRedirects = false
            connection.connectTimeout = 5_000
            connection.readTimeout = 5_000
            connection.requestMethod = "PUT"
            connection.doOutput = true
            connection.setRequestProperty("Authorization", "Bearer ${settings.wakeRegistrationKey}")
            connection.setRequestProperty("Content-Type", "application/json")
            val payload = JSONObject().put("installation_id", SettingsStore(context).deviceId())
                .put("token", token).toString().toByteArray(Charsets.UTF_8)
            connection.outputStream.use { it.write(payload) }
            if (connection.responseCode !in 200..299) throw IllegalStateException("server returned HTTP ${connection.responseCode}")
        } finally {
            connection.disconnect()
        }
    }
}
