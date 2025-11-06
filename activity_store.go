package main

import (
	"sync"
	"time"
)

// --- GLOBAL STATE: Activity Stores ---
var (
	ActivityStore = make(map[TrackingKey]*BotActivity)
	ActivityMutex sync.RWMutex

	DryRunActivityStore = make(map[TrackingKey]*BotActivity)
	DryRunActivityMutex sync.RWMutex
)

// Non-locking variant used when caller already holds the mutex.
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
// NOTE: This function still uses global state variables. It should be refactored to use a *Processor as well.
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
func CleanUpIdleActivity(p *Processor) {
	if p.DryRun {
		return
	}

	p.LogFunc(LevelDebug, "CLEANUP", "Starting Cleanup routine. Purging state older than %v every %v.", IdleTimeout, CleanupInterval)
	for {
		time.Sleep(CleanupInterval)

		p.ActivityMutex.Lock()
		now := time.Now()
		deletedCount := 0

		for trackingKey, activity := range p.ActivityStore {
			if now.Sub(activity.LastRequestTime) > IdleTimeout {
				if trackingKey.UA != "" {
					p.LogFunc(LevelDebug, "CLEANUP", "Purging idle key: %s (UA: %s)", trackingKey.IPInfo.Address, trackingKey.UA)
				} else {
					p.LogFunc(LevelDebug, "CLEANUP", "Purging idle IP: %s", trackingKey.IPInfo.Address)
				}
				delete(p.ActivityStore, trackingKey)
				deletedCount++
			}
		}
		p.ActivityMutex.Unlock()

		if deletedCount > 0 {
			p.LogFunc(LevelDebug, "CLEANUP", "Complete: Purged %d idle IP states. Current active keys: %d", deletedCount, len(p.ActivityStore))
		}
	}
}
