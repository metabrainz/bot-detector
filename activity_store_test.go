package main

import (
	"bot-detector/internal/utils"
	"testing"
	"time"
)

func TestCleanUpIdleActivity(t *testing.T) {
	// 1. Setup
	resetGlobalState()
	cleanupDoneSignal := make(chan struct{}, 1)

	processor := newTestProcessor(&AppConfig{
		CleanupInterval:     10 * time.Millisecond,  // A very short cleanup interval for the test
		IdleTimeout:         100 * time.Millisecond, // A short general timeout
		MaxTimeSinceLastHit: 50 * time.Millisecond,  // A shorter time-based rule timeout
	}, nil)
	processor.TestSignals = &TestSignals{
		CleanupDoneSignal: cleanupDoneSignal,
	}

	// 2. Create different activity states
	now := time.Now()
	actorUseless := Actor{IPInfo: utils.NewIPInfo("192.0.2.1")}     // Will be older than MaxTimeSinceLastHit
	actorStillUseful := Actor{IPInfo: utils.NewIPInfo("192.0.2.2")} // Will be recent
	actorIdle := Actor{IPInfo: utils.NewIPInfo("192.0.2.3")}        // Will be older than IdleTimeout
	actorBlocked := Actor{IPInfo: utils.NewIPInfo("192.0.2.4")}     // Blocked, should not be cleaned up
	actorStaleChain := Actor{IPInfo: utils.NewIPInfo("192.0.2.5")}  // Has chain progress, but it's stale

	processor.ActivityMutex.Lock()
	processor.ActivityStore[actorUseless] = &ActorActivity{LastRequestTime: now.Add(-60 * time.Millisecond)}
	processor.ActivityStore[actorStillUseful] = &ActorActivity{LastRequestTime: now.Add(-20 * time.Millisecond)}
	processor.ActivityStore[actorIdle] = &ActorActivity{LastRequestTime: now.Add(-110 * time.Millisecond)}
	processor.ActivityStore[actorBlocked] = &ActorActivity{LastRequestTime: now.Add(-200 * time.Millisecond), IsBlocked: true}
	processor.ActivityStore[actorStaleChain] = &ActorActivity{
		LastRequestTime: now.Add(-110 * time.Millisecond), // The overall activity is idle
		ChainProgress: map[string]StepState{
			"StaleChain": {
				CurrentStep:   1,
				LastMatchTime: now.Add(-120 * time.Millisecond), // The chain step is older than IdleTimeout
			},
		},
	}
	processor.ActivityMutex.Unlock()

	// --- Act ---
	// Start the cleanup routine and let it run for a few cycles
	stopChan := make(chan struct{})
	go CleanUpIdleActors(processor, stopChan)
	defer close(stopChan)

	// Wait for the cleanup routine to signal it has completed a pass.
	select {
	case <-cleanupDoneSignal:
		// Cleanup finished.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for cleanup routine to complete.")
	}

	// --- Assert ---
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	if _, exists := processor.ActivityStore[actorUseless]; exists {
		t.Error("Expected 'useless' key to be cleaned up by MaxTimeSinceLastHit, but it still exists.")
	}
	if _, exists := processor.ActivityStore[actorIdle]; exists {
		t.Error("Expected 'idle' key to be cleaned up by IdleTimeout, but it still exists.")
	}
	if _, exists := processor.ActivityStore[actorStaleChain]; exists {
		t.Error("Expected key with stale chain progress to be cleaned up, but it still exists.")
	}
	if _, exists := processor.ActivityStore[actorStillUseful]; !exists {
		t.Error("Expected 'still useful' key to remain, but it was cleaned up.")
	}
	if _, exists := processor.ActivityStore[actorBlocked]; !exists {
		t.Error("Expected 'blocked' key to remain, but it was cleaned up.")
	}
}

// TestCleanUpIdleActivity_ImmediateShutdown verifies that the cleanup goroutine
// exits immediately if a stop signal is received before the first tick.
func TestCleanUpIdleActivity_ImmediateShutdown(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	processor := newTestProcessor(&AppConfig{
		CleanupInterval: 1 * time.Second,
	}, nil)

	stopChan := make(chan struct{})
	doneChan := make(chan struct{})

	// --- Act ---
	go func() {
		CleanUpIdleActors(processor, stopChan)
		close(doneChan) // Signal that the goroutine has exited.
	}()

	close(stopChan) // Immediately send the stop signal.
	<-doneChan      // Wait for the goroutine to finish. This will hang if the stop signal is not handled correctly.
}

func TestCleanUpIdleActivity_MinTimeSinceLastHit(t *testing.T) {
	// This test specifically validates the cleanup logic for actors that are no longer
	// relevant for `min_time_since_last_hit` rules because they have been idle for too long.

	// 1. Setup
	resetGlobalState()

	processor := newTestProcessor(&AppConfig{
		// A general idle timeout that is very long.
		IdleTimeout: 1 * time.Hour,
		// A specific, shorter timeout for the time-based rule optimization.
		MaxTimeSinceLastHit: 5 * time.Minute,
		CleanupInterval:     10 * time.Millisecond,
	}, nil)

	// 2. Create different activity states
	now := time.Now()
	// This actor was last seen 6 minutes ago, which is > MaxTimeSinceLastHit. It should be cleaned up.
	actorUselessForTimeRule := Actor{IPInfo: utils.NewIPInfo("192.0.2.10")}
	// This actor was last seen 4 minutes ago, which is < MaxTimeSinceLastHit. It should be kept.
	actorStillRelevantForTimeRule := Actor{IPInfo: utils.NewIPInfo("192.0.2.20")}

	processor.ActivityMutex.Lock()
	processor.ActivityStore[actorUselessForTimeRule] = &ActorActivity{LastRequestTime: now.Add(-6 * time.Minute)}
	processor.ActivityStore[actorStillRelevantForTimeRule] = &ActorActivity{LastRequestTime: now.Add(-4 * time.Minute)}
	processor.ActivityMutex.Unlock()

	// --- Act ---
	stopChan := make(chan struct{})
	go CleanUpIdleActors(processor, stopChan)

	// Wait long enough for the ticker to fire at least once.
	time.Sleep(processor.Config.CleanupInterval * 2)

	close(stopChan)

	// --- Assert ---
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	if _, exists := processor.ActivityStore[actorUselessForTimeRule]; exists {
		t.Error("Expected key older than MaxTimeSinceLastHit to be cleaned up, but it still exists.")
	}
	if _, exists := processor.ActivityStore[actorStillRelevantForTimeRule]; !exists {
		t.Error("Expected key still relevant for MaxTimeSinceLastHit to remain, but it was cleaned up.")
	}
}
