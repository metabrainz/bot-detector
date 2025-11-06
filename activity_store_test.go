package main

import (
	"sync"
	"testing"
	"time"
)

func TestPurgeIdleActivities(t *testing.T) {
	// --- Setup ---
	resetGlobalState()
	t.Cleanup(resetGlobalState)

	// Set a short, testable IdleTimeout for the test.
	testIdleTimeout := 100 * time.Millisecond

	// Create a processor with a fresh activity store
	processor := &Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		// Set the IdleTimeout on the processor's config.
		Config: &AppConfig{IdleTimeout: testIdleTimeout},
	}

	// Define keys for an active and an idle entry
	activeKey := TrackingKey{IPInfo: NewIPInfo("192.0.2.1")}
	idleKey := TrackingKey{IPInfo: NewIPInfo("192.0.2.2")}

	// Populate the store
	processor.ActivityStore[activeKey] = &BotActivity{
		LastRequestTime: time.Now().Add(-50 * time.Millisecond), // Active (50ms ago < 100ms timeout)
	}
	processor.ActivityStore[idleKey] = &BotActivity{
		LastRequestTime: time.Now().Add(-200 * time.Millisecond), // Idle (200ms ago > 100ms timeout)
	}

	// Verify initial state
	if len(processor.ActivityStore) != 2 {
		t.Fatalf("Initial store state is incorrect. Expected 2 entries, got %d", len(processor.ActivityStore))
	}

	// --- Act ---
	deletedCount := purgeIdleActivities(processor)

	// --- Assert ---
	if deletedCount != 1 {
		t.Errorf("Expected purgeIdleActivities to delete 1 entry, but it deleted %d", deletedCount)
	}

	// Final check of the store's contents
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	if len(processor.ActivityStore) != 1 {
		t.Fatalf("Final store state is incorrect. Expected 1 entry, got %d", len(processor.ActivityStore))
	}

	// Check that the idle key was deleted
	if _, exists := processor.ActivityStore[idleKey]; exists {
		t.Error("Expected idle key to be purged, but it still exists.")
	}

	// Check that the active key remains
	if _, exists := processor.ActivityStore[activeKey]; !exists {
		t.Error("Active key was unexpectedly purged from the store.")
	}
}
