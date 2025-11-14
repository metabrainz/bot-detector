package store

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/utils"
	"fmt"
	"strings" // Added for strings.SplitN
	"sync"
	"time"
)

// Provider defines the interface that the store package needs to access application-level
// configuration, metrics, and state. This is a form of dependency injection that allows
// the store package to remain decoupled from the main application package.
type Provider interface {
	GetCleanupInterval() time.Duration
	GetIdleTimeout() time.Duration
	GetMaxTimeSinceLastHit() time.Duration
	GetTopN() int
	GetTopActorsPerChain() map[string]map[string]*ActorStats
	GetActivityStore() map[Actor]*ActorActivity
	GetActivityMutex() *sync.RWMutex
	GetTestSignals() *TestSignals
	IncrementActorsCleaned()
	Log(level logging.LogLevel, tag string, format string, v ...interface{})
}

// TestSignals is a local copy for the provider interface.
type TestSignals struct {
	CleanupDoneSignal chan struct{}
}

// Actor is a comparable struct used as the key for the ActivityStore map.
type Actor struct {
	IPInfo utils.IPInfo
	UA     string
}

// String provides a clean, readable representation of the Actor for logging.
func (a Actor) String() string {
	if a.UA != "" {
		return fmt.Sprintf("%s | %s", a.IPInfo.Address, a.UA)
	}
	return a.IPInfo.Address
}

// NewActorFromString parses an actor string (e.g., "192.168.1.1 | Mozilla/5.0") into an Actor struct.
func NewActorFromString(actorStr string) (Actor, error) {
	parts := strings.SplitN(actorStr, " | ", 2) // Note the " | " separator
	if len(parts) == 0 {
		return Actor{}, fmt.Errorf("invalid actor string: %s", actorStr)
	}

	ipInfo := utils.NewIPInfo(parts[0])
	if ipInfo.Version == utils.VersionInvalid {
		return Actor{}, fmt.Errorf("invalid IP in actor string '%s'", actorStr)
	}

	actor := Actor{IPInfo: ipInfo}
	if len(parts) == 2 {
		actor.UA = parts[1]
	}
	return actor, nil
}

// ActorActivity tracks state for a single actor.
type ActorActivity struct {
	LastRequestTime time.Time
	BlockedUntil    time.Time
	LastUnblockTime time.Time
	ChainProgress   map[string]StepState
	IsBlocked       bool
	SkipInfo        SkipInfo
}

// StepState holds the progress of an actor within a single behavioral chain.
type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// SkipInfo holds structured information about why an actor was skipped.
type SkipInfo struct {
	Type   utils.SkipType
	Source string
}

// ActorStats holds hit and completion counts for a specific actor in a chain.
type ActorStats struct {
	Hits        int64
	Completions int64
	Resets      int64
}

// IsMoreActiveThan compares two ActorStats and returns true if the receiver
// is considered more active than the other. The comparison is done in order
// of priority: Hits > Completions > Resets.
func (s *ActorStats) IsMoreActiveThan(other *ActorStats) bool {
	if s == nil || other == nil {
		return false
	}

	// Primary sort by Hits (descending)
	if s.Hits != other.Hits {
		return s.Hits > other.Hits
	}
	// Secondary sort by Completions (descending)
	if s.Completions != other.Completions {
		return s.Completions > other.Completions
	}
	// Tertiary sort by Resets (descending)
	return s.Resets > other.Resets
}

// GetOrCreateUnsafe finds or creates an ActorActivity without locking.
func GetOrCreateUnsafe(store map[Actor]*ActorActivity, actor Actor) *ActorActivity {
	if activity, exists := store[actor]; exists {
		return activity
	}
	newActivity := &ActorActivity{
		LastRequestTime: time.Time{},
		ChainProgress:   make(map[string]StepState),
	}
	store[actor] = newActivity
	return newActivity
}

// CleanUpIdleActors periodically cleans the activity store.
func CleanUpIdleActors(p Provider, stop <-chan struct{}) {
	cleanupInterval := p.GetCleanupInterval()

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		activityStore := p.GetActivityStore()
		activityMutex := p.GetActivityMutex()

		activityMutex.Lock()
		now := time.Now()
		cleanedCount := 0
		for key, activityPtr := range activityStore {
			activity := activityPtr

			if activity.IsBlocked {
				continue
			}

			hasActiveChain := false
			for _, state := range activity.ChainProgress {
				if now.Sub(state.LastMatchTime) < p.GetIdleTimeout() {
					hasActiveChain = true
					break
				}
			}

			if !hasActiveChain {
				timeSinceLastHit := now.Sub(activity.LastRequestTime)
				isIdle := timeSinceLastHit > p.GetIdleTimeout()
				isUselessForTimeRule := p.GetMaxTimeSinceLastHit() > 0 && timeSinceLastHit > p.GetMaxTimeSinceLastHit()

				if isIdle || isUselessForTimeRule {
					delete(activityStore, key)
					if p.GetTopN() > 0 {
						actorString := key.String()
						for _, chainStats := range p.GetTopActorsPerChain() {
							delete(chainStats, actorString)
						}
					}
					p.IncrementActorsCleaned()
					cleanedCount++
				}
			}
		}
		activityMutex.Unlock()

		if cleanedCount > 0 {
			p.Log(logging.LevelDebug, "CLEANUP", "Cleaned up %d idle/useless actor states.", cleanedCount)
		}

		if signals := p.GetTestSignals(); signals != nil && signals.CleanupDoneSignal != nil {
			signals.CleanupDoneSignal <- struct{}{}
		}
	}
}
