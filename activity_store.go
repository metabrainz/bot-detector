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

// CleanUpIdleActivity periodically iterates through the ActivityStore and removes entries
// for IPs that have been inactive for longer than the configured IdleTimeout or have become
// irrelevant for `first_hit_since` checks.
func (p *Processor) CleanUpIdleActivity() {
	// Enforce a minimum cleanup interval to prevent a tight loop on a zero-value duration.
	cleanupInterval := p.Config.CleanupInterval
	if cleanupInterval < 1*time.Second {
		cleanupInterval = 1 * time.Minute // Default to a safe interval.
	}

	// Create a ticker that fires at the specified interval.
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		p.ActivityMutex.Lock()
		now := time.Now()
		cleanedCount := 0
		for key, activity := range p.ActivityStore {
			// Only consider cleanup for IPs that are not currently blocked and not in the middle of a chain.
			if !activity.IsBlocked && len(activity.ChainProgress) == 0 {
				timeSinceLastHit := now.Sub(activity.LastRequestTime)
				// Condition 1: General idle timeout.
				isIdle := timeSinceLastHit > p.Config.IdleTimeout
				// Condition 2: Useless for first_hit_since checks.
				isUselessForFirstHit := p.Config.MaxFirstHitSinceDuration > 0 && timeSinceLastHit > p.Config.MaxFirstHitSinceDuration

				if isIdle || isUselessForFirstHit {
					delete(p.ActivityStore, key)
					cleanedCount++
				}
			}
		}
		p.ActivityMutex.Unlock()
		if cleanedCount > 0 {
			p.LogFunc(LevelDebug, "CLEANUP", "Cleaned up %d idle/useless IP states.", cleanedCount)
		}
	}
}
