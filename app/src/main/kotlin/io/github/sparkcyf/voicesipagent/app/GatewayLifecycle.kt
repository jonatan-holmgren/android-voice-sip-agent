package io.github.sparkcyf.voicesipagent.app

import java.util.LinkedHashSet

enum class GatewayLifecycle {
    DORMANT, WAKING, INITIALIZING, REGISTERING, READY, IN_CALL, IDLE_GRACE, STOPPING, ERROR,
}

internal enum class WakeRequestDecision { ACCEPTED, DUPLICATE, EXPIRED, INVALID }

/** Bounded persistent FCM replay protection. The backend remains authoritative for request expiry. */
internal class WakeRequestTracker(
    private val readIds: () -> Set<String>,
    private val record: (String, Set<String>, Long) -> Unit,
) {
    @Synchronized fun accept(requestId: String, expiresAtEpochMs: Long, nowEpochMs: Long): WakeRequestDecision {
        if (!requestId.matches(Regex("[A-Za-z0-9_-]{16,96}"))) return WakeRequestDecision.INVALID
        if (expiresAtEpochMs <= nowEpochMs || expiresAtEpochMs - nowEpochMs > MAX_AGE_MS) return WakeRequestDecision.EXPIRED
        val ids = LinkedHashSet(readIds())
        if (requestId in ids) return WakeRequestDecision.DUPLICATE
        ids += requestId
        record(requestId, ids, expiresAtEpochMs)
        return WakeRequestDecision.ACCEPTED
    }

    companion object {
        const val MAX_IDS = 128
        const val MAX_AGE_MS = 120_000L
    }
}
