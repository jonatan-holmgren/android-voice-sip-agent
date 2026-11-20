package io.github.sparkcyf.voicesipagent.app

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

class WakeRequestTrackerTest {
    @Test fun rejectsExpiredMalformedAndOverlongRequests() {
        val tracker = tracker()
        assertEquals(WakeRequestDecision.EXPIRED, tracker.accept("request-000000001", 10_000, 10_000))
        assertEquals(WakeRequestDecision.INVALID, tracker.accept("short", 11_000, 10_000))
        assertEquals(WakeRequestDecision.EXPIRED, tracker.accept("request-000000001", 131_000, 10_000))
    }

    @Test fun wakeIdsAreDurablyDeduplicated() {
        var saved = emptySet<String>()
        val tracker = WakeRequestTracker({ saved }, { _, ids, _ -> saved = ids })
        assertEquals(WakeRequestDecision.ACCEPTED, tracker.accept("request-000000001", 11_000, 10_000))
        assertEquals(WakeRequestDecision.DUPLICATE, tracker.accept("request-000000001", 11_000, 10_000))
        assertTrue("request-000000002" in saved)
    }

    @Test fun trackerBoundsReplayHistory() {
        var saved = emptySet<String>()
        val tracker = WakeRequestTracker({ saved }, { _, ids, _ -> saved = ids })
        repeat(WakeRequestTracker.MAX_IDS + 1) { index ->
            assertEquals(WakeRequestDecision.ACCEPTED,
                tracker.accept("request-${index.toString().padStart(9, '0')}", 11_000, 10_000))
        }
        assertTrue(saved.size <= WakeRequestTracker.MAX_IDS)
    }

    @Test fun concurrentDuplicateMessagesAreAcceptedExactlyOnce() {
        val executor = Executors.newFixedThreadPool(8)
        try {
            var saved = emptySet<String>()
            val tracker = WakeRequestTracker({ synchronized(this) { saved } }, { _, ids, _ ->
                synchronized(this) { saved = ids }
            })
            val start = CountDownLatch(1)
            val done = CountDownLatch(8)
            val accepted = AtomicInteger()
            repeat(8) {
                executor.execute {
                    start.await()
                    if (tracker.accept("request-concurrent-0001", 11_000, 10_000) == WakeRequestDecision.ACCEPTED) {
                        accepted.incrementAndGet()
                    }
                    done.countDown()
                }
            }
            start.countDown()
            assertTrue(done.await(2, TimeUnit.SECONDS))
            assertEquals(1, accepted.get())
        } finally {
            executor.shutdownNow()
        }
    }

    private fun tracker(): WakeRequestTracker {
        var saved = emptySet<String>()
        return WakeRequestTracker({ saved }, { _, ids, _ -> saved = ids })
    }
}
