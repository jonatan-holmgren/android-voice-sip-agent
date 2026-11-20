package io.github.sparkcyf.voicesipagent.app

import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage

/** Keep the callback bounded: validate the envelope, record it, and request the foreground service. */
class GatewayMessagingService : FirebaseMessagingService() {
    override fun onNewToken(token: String) {
        // Token contents are sensitive. FcmPush obtains the current token and never logs it.
        FcmPush.refresh(this, GatewayRuntime.get(this).settings)
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val runtime = GatewayRuntime.get(this)
        if (message.priority != RemoteMessage.PRIORITY_HIGH) {
            AppLog.w("FCM wake not started: Android delivered non-high priority")
            runtime.reportWakeFailure("Failed: FCM priority downgraded")
            return
        }
        val data = message.data
        if (data["type"] != "gateway_wake") {
            AppLog.w("FCM message ignored: unsupported type")
            runtime.reportWakeFailure("Failed: unsupported FCM message")
            return
        }
        val requestId = data["request_id"] ?: run {
            AppLog.w("FCM wake missing request id")
            runtime.reportWakeFailure("Failed: wake request id missing")
            return
        }
        val expiresAtMs = data["expires_at_ms"]?.toLongOrNull()
            ?: run {
                AppLog.w("FCM wake has invalid expiry")
                runtime.reportWakeFailure("Failed: invalid wake expiry")
                return
            }
        if (!runtime.requestWake(requestId, expiresAtMs)) {
            AppLog.i("FCM wake rejected or service startup blocked; see gateway diagnostics")
        }
    }

    override fun onDeletedMessages() {
        AppLog.w("FCM deleted pending messages; gateway wake may require a retry from Asterisk")
    }
}
