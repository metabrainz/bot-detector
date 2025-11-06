package main

import "time"

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

// purgeIdleActivities contains the core logic for iterating through the activity store
// and removing entries that have been idle for longer than the IdleTimeout.
// This function is separate from the infinite loop to allow for direct testing.
func purgeIdleActivities(p *Processor) int {
	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()

	now := time.Now()
	deletedCount := 0

	for trackingKey, activity := range p.ActivityStore {
		if now.Sub(activity.LastRequestTime) > p.Config.IdleTimeout {
			if trackingKey.UA != "" {
				p.LogFunc(LevelDebug, "CLEANUP", "Purging idle key: %s (UA: %s)", trackingKey.IPInfo.Address, trackingKey.UA)
			} else {
				p.LogFunc(LevelDebug, "CLEANUP", "Purging idle IP: %s", trackingKey.IPInfo.Address)
			}
			delete(p.ActivityStore, trackingKey)
			deletedCount++
		}
	}
	return deletedCount
}

// CleanUpIdleActivity periodically purges state for IPs inactive longer than IdleTimeout.
func (p *Processor) CleanUpIdleActivity() {
	if p.DryRun {
		return
	}

	// Enforce a minimum cleanup interval to prevent a tight loop on a zero-value duration.
	cleanupInterval := p.Config.CleanupInterval
	if cleanupInterval < 1*time.Second {
		cleanupInterval = 1 * time.Minute // Default to a safe interval.
	}

	p.LogFunc(LevelDebug, "CLEANUP", "Starting Cleanup routine. Purging state older than %v every %v.", p.Config.IdleTimeout, cleanupInterval)
	for {
		time.Sleep(cleanupInterval)
		deletedCount := purgeIdleActivities(p)
		if deletedCount > 0 {
			p.LogFunc(LevelDebug, "CLEANUP", "Complete: Purged %d idle IP states. Current active keys: %d", deletedCount, len(p.ActivityStore))
		}
	}
}
