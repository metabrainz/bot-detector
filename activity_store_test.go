package main

import (
	"sync"
	"testing"
	"time"
)

func TestCleanUpIdleActivity(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	// Create a processor with specific timeout values for the test.
	processor := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ChainMutex:    &sync.RWMutex{},
		Chains:        []BehavioralChain{}, // No chains needed for this test
		Config: &AppConfig{
			CleanupInterval:     10 * time.Millisecond,  // A very short cleanup interval for the test
			IdleTimeout:         100 * time.Millisecond, // A short general timeout
			MaxTimeSinceLastHit: 50 * time.Millisecond,  // A shorter time-based rule timeout
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}

	// 2. Create different activity states
	now := time.Now()
	keyUseless := TrackingKey{IPInfo: NewIPInfo("192.0.2.1")}     // Will be older than MaxTimeSinceLastHit
	keyStillUseful := TrackingKey{IPInfo: NewIPInfo("192.0.2.2")} // Will be recent
	keyIdle := TrackingKey{IPInfo: NewIPInfo("192.0.2.3")}        // Will be older than IdleTimeout
	keyBlocked := TrackingKey{IPInfo: NewIPInfo("192.0.2.4")}     // Blocked, should not be cleaned up
	keyStaleChain := TrackingKey{IPInfo: NewIPInfo("192.0.2.5")}  // Has chain progress, but it's stale

	processor.ActivityMutex.Lock()
	processor.ActivityStore[keyUseless] = &BotActivity{LastRequestTime: now.Add(-60 * time.Millisecond)}
	processor.ActivityStore[keyStillUseful] = &BotActivity{LastRequestTime: now.Add(-20 * time.Millisecond)}
	processor.ActivityStore[keyIdle] = &BotActivity{LastRequestTime: now.Add(-110 * time.Millisecond)}
	processor.ActivityStore[keyBlocked] = &BotActivity{LastRequestTime: now.Add(-200 * time.Millisecond), IsBlocked: true}
	processor.ActivityStore[keyStaleChain] = &BotActivity{
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
	go processor.CleanUpIdleActivity(stopChan)

	// Wait long enough for the ticker to fire at least once.
	time.Sleep(processor.Config.CleanupInterval * 2)

	// Stop the cleanup goroutine
	close(stopChan)

	// --- Assert ---
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	if _, exists := processor.ActivityStore[keyUseless]; exists {
		t.Error("Expected 'useless' key to be cleaned up by MaxTimeSinceLastHit, but it still exists.")
	}
	if _, exists := processor.ActivityStore[keyIdle]; exists {
		t.Error("Expected 'idle' key to be cleaned up by IdleTimeout, but it still exists.")
	}
	if _, exists := processor.ActivityStore[keyStaleChain]; exists {
		t.Error("Expected key with stale chain progress to be cleaned up, but it still exists.")
	}
	if _, exists := processor.ActivityStore[keyStillUseful]; !exists {
		t.Error("Expected 'still useful' key to remain, but it was cleaned up.")
	}
	if _, exists := processor.ActivityStore[keyBlocked]; !exists {
		t.Error("Expected 'blocked' key to remain, but it was cleaned up.")
	}
}

// TestCleanUpIdleActivity_ImmediateShutdown verifies that the cleanup goroutine
// exits immediately if a stop signal is received before the first tick.
func TestCleanUpIdleActivity_ImmediateShutdown(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	processor := &Processor{
		Config: &AppConfig{
			// Use a long interval so the tick won't fire during the test.
			CleanupInterval: 1 * time.Second,
		},
	}

	stopChan := make(chan struct{})
	doneChan := make(chan struct{})

	// --- Act ---
	go func() {
		processor.CleanUpIdleActivity(stopChan)
		close(doneChan) // Signal that the goroutine has exited.
	}()

	close(stopChan) // Immediately send the stop signal.
	<-doneChan      // Wait for the goroutine to finish. This will hang if the stop signal is not handled correctly.
}

func TestCleanUpIdleActivity_MinTimeSinceLastHit(t *testing.T) {
	// This test specifically validates the cleanup logic for IPs that are no longer
	// relevant for `min_time_since_last_hit` rules.

	// 1. Setup
	resetGlobalState()

	processor := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		Config: &AppConfig{
			// A general idle timeout that is very long.
			IdleTimeout: 1 * time.Hour,
			// A specific, shorter timeout for the time-based rule optimization.
			MaxTimeSinceLastHit: 5 * time.Minute,
			CleanupInterval:     10 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}

	// 2. Create different activity states
	now := time.Now()
	// This IP was last seen 6 minutes ago, which is > MaxTimeSinceLastHit. It should be cleaned up.
	keyUselessForTimeRule := TrackingKey{IPInfo: NewIPInfo("192.0.2.10")}
	// This IP was last seen 4 minutes ago, which is < MaxTimeSinceLastHit. It should be kept.
	keyStillRelevantForTimeRule := TrackingKey{IPInfo: NewIPInfo("192.0.2.20")}

	processor.ActivityMutex.Lock()
	processor.ActivityStore[keyUselessForTimeRule] = &BotActivity{LastRequestTime: now.Add(-6 * time.Minute)}
	processor.ActivityStore[keyStillRelevantForTimeRule] = &BotActivity{LastRequestTime: now.Add(-4 * time.Minute)}
	processor.ActivityMutex.Unlock()

	// --- Act ---
	stopChan := make(chan struct{})
	go processor.CleanUpIdleActivity(stopChan)

	// Wait long enough for the ticker to fire at least once.
	time.Sleep(processor.Config.CleanupInterval * 2)

	close(stopChan)

	// --- Assert ---
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	if _, exists := processor.ActivityStore[keyUselessForTimeRule]; exists {
		t.Error("Expected key older than MaxTimeSinceLastHit to be cleaned up, but it still exists.")
	}
	if _, exists := processor.ActivityStore[keyStillRelevantForTimeRule]; !exists {
		t.Error("Expected key still relevant for MaxTimeSinceLastHit to remain, but it was cleaned up.")
	}
}
