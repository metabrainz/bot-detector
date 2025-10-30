package main

import (
	"sync"
	"time"
)

// --- GLOBAL STATE: Activity Stores ---
var (
	ActivityStore = make(map[TrackingKey]*BotActivity)
	ActivityMutex sync.Mutex // Mutex protecting concurrent access to ActivityStore.

	DryRunActivityStore = make(map[TrackingKey]*BotActivity)
	DryRunActivityMutex sync.Mutex
)

// New helper: non-locking variant used when caller already holds the mutex.
func GetOrCreateActivityUnsafe(store map[TrackingKey]*BotActivity, trackingKey TrackingKey) *BotActivity {
	if activity, exists := store[trackingKey]; exists {
		return activity
	}
	newActivity := &BotActivity{
		LastRequestTime: time.Time{},
		ChainProgress:   make(map[string]StepState),
	}
	store[trackingKey] = newActivity
	return newActivity
}

// GetOrCreateActivity retrieves or initializes a BotActivity struct for a given tracking key, ensuring thread safety.
func GetOrCreateActivity(trackingKey TrackingKey) *BotActivity {
	store := ActivityStore
	mutex := &ActivityMutex

	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	}

	mutex.Lock()
	defer mutex.Unlock()

	return GetOrCreateActivityUnsafe(store, trackingKey)
}

// CleanUpIdleActivity periodically purges state for IPs inactive longer than IdleTimeout.
func CleanUpIdleActivity() {
	if DryRun {
		return
	}

	LogOutput(LevelDebug, "CLEANUP", "Starting Cleanup routine. Purging state older than %v every %v.", IdleTimeout, CleanupInterval)
	for {
		time.Sleep(CleanupInterval)

		ActivityMutex.Lock()
		now := time.Now()
		deletedCount := 0

		for trackingKey, activity := range ActivityStore {
			if now.Sub(activity.LastRequestTime) > IdleTimeout {
				if trackingKey.UA != "" {
					LogOutput(LevelDebug, "CLEANUP", "Purging idle key: %s (UA: %s)", trackingKey.IP, trackingKey.UA)
				} else {
					LogOutput(LevelDebug, "CLEANUP", "Purging idle IP: %s", trackingKey.IP)
				}
				delete(ActivityStore, trackingKey)
				deletedCount++
			}
		}
		ActivityMutex.Unlock()

		if deletedCount > 0 {
			LogOutput(LevelDebug, "CLEANUP", "Complete: Purged %d idle IP states. Current active keys: %d", deletedCount, len(ActivityStore))
		}
	}
}