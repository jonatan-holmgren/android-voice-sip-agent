package io.github.sparkcyf.voicesipagent.app

import android.content.Context
import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import io.github.sparkcyf.voicesipagent.gateway.BusyPolicy
import io.github.sparkcyf.voicesipagent.pjsua2.TlsCertificates
import io.github.sparkcyf.voicesipagent.privileged.AudioMethod
import io.github.sparkcyf.voicesipagent.privileged.CallAccess
import io.github.sparkcyf.voicesipagent.telephony.DefaultSim
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

data class GatewaySettings(
    /** Master switch; call cleanup and default-dialer restoration may finish later. */
    val takeover: Boolean = true,
    /** Keep SIP dormant between gateway requests; FCM wakes the foreground service on demand. */
    val onDemandSip: Boolean = false,
    /** Grace after the last call before SIP and its native library are released. */
    val idleTimeoutSeconds: Int = 60,
    val wakeServerUrl: String = "",
    val wakeRegistrationKey: String = "",
    /** Retain the role on switch-off instead of returning to the saved dialer. */
    val keepDefaultDialer: Boolean = false,
    /** Mute local call output while bridged. Telecom manages microphone mute independently. */
    val muteEarpiece: Boolean = true,
    val server: String = "",
    val port: Int = 5060,
    /** udp | tcp | tls; the built-in stack only supports udp. */
    val sipTransport: String = "udp",
    /** off | optional | mandatory SDES-SRTP (pjsua2 over TLS only). */
    val srtp: String = "optional",
    /** TLS only: verify the PBX certificate. Off accepts any certificate (pjsua2). */
    val tlsVerify: Boolean = true,
    /** TLS only: PEM bundle trusted instead of the Android system CAs; empty = system CAs (pjsua2). */
    val tlsCa: String = "",
    /** pjsip info-level logging (development); errors/warnings are always logged. */
    val sipDebugLog: Boolean = false,
    /** Keep the SIP account unregistered while the cellular side is not ready, so the PBX can fail over. */
    val holdSipWhenNotReady: Boolean = true,
    val username: String = "",
    val password: String = "",
    val domain: String = "",
    val localSipPort: Int = 5062,
    /** SIP extension / DID that receives cellular inbound calls; blank = the SIP username (trunk DID). */
    val inboundTarget: String = "",
    /** Comma-separated; empty = any number may be dialed out. */
    val allowedPrefixes: String = "",
    val busyPolicy: BusyPolicy = BusyPolicy.REJECT,
    val audioMethod: Int = AudioMethod.AUTO,
    val callAccess: Int = CallAccess.AUTO,
    /** Which SIP implementation backs the PBX side. */
    val sipStack: SipStack = SipStack.PJSUA2,
    /** Outbound SIM for PBX calls without X-VSA-SIMSLOT; see [DefaultSim]. */
    val defaultSimSlot: Int = DefaultSim.SYSTEM,
    /** SIM slots whose incoming calls stay with the phone app; new SIMs are taken over. */
    val inboundExcludedSlots: Set<Int> = emptySet(),
) {
    val sipConfigured: Boolean get() = server.isNotBlank() && username.isNotBlank() && password.isNotEmpty()

    override fun toString() =
        "GatewaySettings(takeover=$takeover onDemandSip=$onDemandSip idleTimeoutSeconds=$idleTimeoutSeconds wakeServerConfigured=${wakeServerUrl.isNotBlank()} wakeKeyConfigured=${wakeRegistrationKey.isNotBlank()} muteEarpiece=$muteEarpiece stack=$sipStack server=$server:$port/$sipTransport srtp=$srtp tlsVerify=$tlsVerify tlsCa=${TlsCertificates.label(tlsCa)} holdSip=$holdSipWhenNotReady user=$username target=$inboundTarget)"
}

enum class SipStack(val id: String) {
    /** The hand-written Kotlin stack in :sip. */
    BUILTIN("builtin"),

    /** pjsip's pjsua2 via :sip-pjsua2. */
    PJSUA2("pjsua2"),
    ;

    companion object {
        fun of(id: String?): SipStack? = entries.firstOrNull { it.id == id }
    }
}

/** SharedPreferences, with the SIP password sealed by an Android Keystore AES-GCM key. */
class SettingsStore(context: Context) {
    private val prefs = context.getSharedPreferences("gateway", Context.MODE_PRIVATE)

    internal fun seenWakeRequests(): Set<String> =
        prefs.getStringSet("wake_request_ids", emptySet())?.toSet().orEmpty()

    internal fun recordWakeRequest(requestId: String, existing: Set<String>, expiresAtEpochMs: Long) {
        val pending = prefs.getStringSet("pending_wake_requests", emptySet())?.toMutableSet().orEmpty()
        pending.removeAll { entry ->
            val expiry = entry.substringAfterLast(':', "").toLongOrNull()
            expiry == null || expiry <= System.currentTimeMillis()
        }
        pending += "$requestId:$expiresAtEpochMs"
        prefs.edit()
            .putStringSet("wake_request_ids", (existing + requestId).takeLast(WakeRequestTracker.MAX_IDS).toSet())
            .putStringSet("pending_wake_requests", pending.takeLast(WakeRequestTracker.MAX_IDS).toSet())
            .apply()
    }

    internal fun pendingWakeRequests(nowEpochMs: Long): Map<String, Long> {
        val raw = prefs.getStringSet("pending_wake_requests", emptySet())?.toSet().orEmpty()
        val pending = raw.mapNotNull { entry ->
            val id = entry.substringBeforeLast(':', "")
            val expiry = entry.substringAfterLast(':', "").toLongOrNull()
            if (id.isNotEmpty() && expiry != null && expiry > nowEpochMs) id to expiry else null
        }.toMap()
        if (pending.size != raw.size) {
            prefs.edit().putStringSet("pending_wake_requests", pending.map { (id, expiry) -> "$id:$expiry" }.toSet()).apply()
        }
        return pending
    }

    internal fun completeWakeRequest(requestId: String) {
        val pending = prefs.getStringSet("pending_wake_requests", emptySet())?.toMutableSet().orEmpty()
        pending.removeAll { it.substringBeforeLast(':', "") == requestId }
        prefs.edit().putStringSet("pending_wake_requests", pending).apply()
    }

    internal fun deviceId(): String = prefs.getString("fcm_device_id", null) ?: java.util.UUID.randomUUID().toString().also {
        prefs.edit().putString("fcm_device_id", it).apply()
    }

    fun load(): GatewaySettings = GatewaySettings(
        takeover = prefs.getBoolean("takeover", true),
        onDemandSip = prefs.getBoolean("on_demand_sip", false),
        idleTimeoutSeconds = prefs.getInt("idle_timeout_seconds", 60).coerceIn(15, 600),
        wakeServerUrl = prefs.getString("wake_server_url", "")!!,
        wakeRegistrationKey = prefs.getString("wake_registration_key", null)?.let(::unseal).orEmpty(),
        keepDefaultDialer = prefs.getBoolean("keep_default_dialer", false),
        muteEarpiece = prefs.getBoolean("mute_earpiece", true),
        server = prefs.getString("server", "")!!,
        port = prefs.getInt("port", 5060),
        sipTransport = prefs.getString("sip_transport", "udp")!!,
        srtp = prefs.getString("srtp", "optional")!!,
        tlsVerify = prefs.getBoolean("tls_verify", true),
        tlsCa = prefs.getString("tls_ca", "")!!,
        sipDebugLog = prefs.getBoolean("sip_debug_log", false),
        holdSipWhenNotReady = prefs.getBoolean("hold_sip_when_not_ready", true),
        username = prefs.getString("username", "")!!,
        password = prefs.getString("password_sealed", null)?.let(::unseal).orEmpty(),
        domain = prefs.getString("domain", "")!!,
        localSipPort = prefs.getInt("local_sip_port", 5062),
        inboundTarget = prefs.getString("inbound_target", "")!!,
        allowedPrefixes = prefs.getString("allowed_prefixes", "")!!,
        busyPolicy = runCatching { BusyPolicy.valueOf(prefs.getString("busy_policy", "REJECT")!!) }.getOrDefault(BusyPolicy.REJECT),
        audioMethod = prefs.getInt("audio_method", AudioMethod.AUTO).takeIf { it in 0..2 } ?: AudioMethod.AUTO,
        callAccess = prefs.getInt("call_access", CallAccess.AUTO).takeIf { it in 0..2 } ?: CallAccess.AUTO,
        sipStack = SipStack.of(prefs.getString("sip_stack", null)) ?: SipStack.PJSUA2,
        defaultSimSlot = prefs.getInt("default_sim_slot", DefaultSim.SYSTEM).takeIf(DefaultSim::isValid) ?: DefaultSim.SYSTEM,
        inboundExcludedSlots = prefs.getString("inbound_excluded_slots", "")!!.split(',')
            .mapNotNull { it.trim().toIntOrNull()?.takeIf { slot -> slot >= 0 } }.toSet(),
    )

    fun save(s: GatewaySettings) {
        prefs.edit()
            .putBoolean("takeover", s.takeover)
            .putBoolean("on_demand_sip", s.onDemandSip)
            .putInt("idle_timeout_seconds", s.idleTimeoutSeconds.coerceIn(15, 600))
            .putString("wake_server_url", s.wakeServerUrl.trim())
            .putString("wake_registration_key", seal(s.wakeRegistrationKey))
            .putBoolean("keep_default_dialer", s.keepDefaultDialer)
            .putBoolean("mute_earpiece", s.muteEarpiece)
            .putString("server", s.server.trim())
            .putInt("port", s.port)
            .putString("sip_transport", s.sipTransport.trim().lowercase())
            .putString("srtp", s.srtp.trim().lowercase())
            .putBoolean("tls_verify", s.tlsVerify)
            .putString("tls_ca", s.tlsCa)
            .putBoolean("sip_debug_log", s.sipDebugLog)
            .putBoolean("hold_sip_when_not_ready", s.holdSipWhenNotReady)
            .putString("username", s.username.trim())
            .putString("password_sealed", seal(s.password))
            .putString("domain", s.domain.trim())
            .putInt("local_sip_port", s.localSipPort)
            .putString("inbound_target", s.inboundTarget.trim())
            .putString("allowed_prefixes", s.allowedPrefixes.trim())
            .putString("busy_policy", s.busyPolicy.name)
            .putInt("audio_method", s.audioMethod)
            .putInt("call_access", s.callAccess)
            .putString("sip_stack", s.sipStack.id)
            .putInt("default_sim_slot", s.defaultSimSlot)
            .putString("inbound_excluded_slots", s.inboundExcludedSlots.sorted().joinToString(","))
            .apply()
    }

    /**
     * Voice-call stream mute stopped direct call capture on this build. Kept apart from
     * [GatewaySettings]: it is an observation, keyed by the build fingerprint so OS updates retest.
     */
    fun directMuteIncompatible(): Boolean = prefs.getString(DIRECT_MUTE_INCOMPATIBLE, null) == Build.FINGERPRINT

    fun setDirectMuteIncompatible(on: Boolean) {
        prefs.edit().apply {
            if (on) putString(DIRECT_MUTE_INCOMPATIBLE, Build.FINGERPRINT) else remove(DIRECT_MUTE_INCOMPATIBLE)
        }.apply()
    }

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        (ks.getKey(KEY_ALIAS, null) as? SecretKey)?.let { return it }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        gen.init(
            KeyGenParameterSpec.Builder(KEY_ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .build(),
        )
        return gen.generateKey()
    }

    private fun seal(plain: String): String {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.ENCRYPT_MODE, key())
        val out = c.iv + c.doFinal(plain.toByteArray())
        return Base64.encodeToString(out, Base64.NO_WRAP)
    }

    private fun unseal(sealed: String): String? = try {
        val raw = Base64.decode(sealed, Base64.NO_WRAP)
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, raw, 0, 12))
        String(c.doFinal(raw, 12, raw.size - 12))
    } catch (e: Exception) {
        AppLog.w("stored SIP password unreadable; re-enter it", e)
        null
    }

    companion object {
        private const val KEY_ALIAS = "sip-password"
        private const val DIRECT_MUTE_INCOMPATIBLE = "direct_mute_incompatible_build"
    }
}
