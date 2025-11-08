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
// irrelevant for `min_time_since_last_hit` checks. It listens on the `stop` channel to exit gracefully.
func (p *Processor) CleanUpIdleActivity(stop <-chan struct{}) {
	cleanupInterval := p.Config.CleanupInterval

	// Create a ticker that fires at the specified interval.
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		p.ActivityMutex.Lock()
		now := time.Now()
		cleanedCount := 0
		for key, activityPtr := range p.ActivityStore {
			// Use a new variable to avoid loop variable capture issues.
			activity := activityPtr
			// Only consider cleanup for IPs that are not currently blocked and not in the middle of a chain.
			if !activity.IsBlocked && len(activity.ChainProgress) == 0 {
				timeSinceLastHit := now.Sub(activity.LastRequestTime)
				// Condition 1: General idle timeout.
				isIdle := timeSinceLastHit > p.Config.IdleTimeout
				// Condition 2: Useless for min_time_since_last_hit checks.
				isUselessForTimeRule := p.Config.MaxTimeSinceLastHit > 0 && timeSinceLastHit > p.Config.MaxTimeSinceLastHit

				if isIdle || isUselessForTimeRule {
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
