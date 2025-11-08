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
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        []BehavioralChain{}, // No chains needed for this test
		ChainMutex:    &sync.RWMutex{},
		LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {},
		Config: &AppConfig{
			IdleTimeout:         100 * time.Millisecond, // A short general timeout
			MaxTimeSinceLastHit: 50 * time.Millisecond,  // A shorter time-based rule timeout
			CleanupInterval:     10 * time.Millisecond,  // A very short cleanup interval for the test
		},
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
