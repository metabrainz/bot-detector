package store

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/utils"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockStoreProvider implements the Provider interface for testing the store package.
type mockStoreProvider struct {
	cleanupInterval     time.Duration
	idleTimeout         time.Duration
	maxTimeSinceLastHit time.Duration
	topN                int
	activityStore       map[Actor]*ActorActivity
	activityMutex       *sync.RWMutex
	topActorsPerChain   map[string]map[string]*ActorStats
	testSignals         *TestSignals
	actorsCleaned       atomic.Int32
}

func (m *mockStoreProvider) GetCleanupInterval() time.Duration          { return m.cleanupInterval }
func (m *mockStoreProvider) GetIdleTimeout() time.Duration              { return m.idleTimeout }
func (m *mockStoreProvider) GetMaxTimeSinceLastHit() time.Duration      { return m.maxTimeSinceLastHit }
func (m *mockStoreProvider) GetTopN() int                               { return m.topN }
func (m *mockStoreProvider) GetActivityStore() map[Actor]*ActorActivity { return m.activityStore }
func (m *mockStoreProvider) GetActivityMutex() *sync.RWMutex            { return m.activityMutex }
func (m *mockStoreProvider) GetTopActorsPerChain() map[string]map[string]*ActorStats {
	return m.topActorsPerChain
}
func (m *mockStoreProvider) GetTestSignals() *TestSignals { return m.testSignals }
func (m *mockStoreProvider) IncrementActorsCleaned()      { m.actorsCleaned.Add(1) }
func (m *mockStoreProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
}

func newMockStoreProvider(idleTimeout, maxTimeSinceLastHit time.Duration) *mockStoreProvider {
	return &mockStoreProvider{
		cleanupInterval:     10 * time.Millisecond,
		idleTimeout:         idleTimeout,
		maxTimeSinceLastHit: maxTimeSinceLastHit,
		activityStore:       make(map[Actor]*ActorActivity),
		activityMutex:       &sync.RWMutex{},
		topActorsPerChain:   make(map[string]map[string]*ActorStats),
		testSignals: &TestSignals{
			CleanupDoneSignal: make(chan struct{}, 1),
		},
	}
}

func TestGetOrCreateUnsafe(t *testing.T) {
	store := make(map[Actor]*ActorActivity)
	key := Actor{IPInfo: utils.NewIPInfo("192.168.1.1")}

	// First call should create a new entry
	activity1 := GetOrCreateUnsafe(store, key)
	if activity1 == nil {
		t.Fatal("Expected a new activity, but got nil")
	}
	if len(store) != 1 {
		t.Errorf("Expected store size to be 1, but got %d", len(store))
	}

	// Second call should return the existing entry
	activity2 := GetOrCreateUnsafe(store, key)
	if activity2 != activity1 {
		t.Error("Expected to get the same activity instance, but got a different one")
	}
	if len(store) != 1 {
		t.Errorf("Expected store size to remain 1, but got %d", len(store))
	}
}

func TestCleanUpIdleActors(t *testing.T) {
	// 1. Setup
	provider := newMockStoreProvider(100*time.Millisecond, 50*time.Millisecond)
	provider.topN = 5 // Enable TopN cleanup for the test

	// 2. Create different activity states
	now := time.Now()
	actorUseless := Actor{IPInfo: utils.NewIPInfo("192.0.2.1")}     // Will be older than MaxTimeSinceLastHit
	actorStillUseful := Actor{IPInfo: utils.NewIPInfo("192.0.2.2")} // Will be recent
	actorIdle := Actor{IPInfo: utils.NewIPInfo("192.0.2.3")}        // Will be older than IdleTimeout
	actorBlocked := Actor{IPInfo: utils.NewIPInfo("192.0.2.4")}     // Blocked, should not be cleaned up
	actorStaleChain := Actor{IPInfo: utils.NewIPInfo("192.0.2.5")}  // Has chain progress, but it's stale

	provider.activityStore[actorUseless] = &ActorActivity{LastRequestTime: now.Add(-60 * time.Millisecond)}
	provider.activityStore[actorStillUseful] = &ActorActivity{LastRequestTime: now.Add(-20 * time.Millisecond)}
	provider.activityStore[actorIdle] = &ActorActivity{LastRequestTime: now.Add(-110 * time.Millisecond)}
	provider.activityStore[actorBlocked] = &ActorActivity{LastRequestTime: now.Add(-200 * time.Millisecond), IsBlocked: true}
	provider.activityStore[actorStaleChain] = &ActorActivity{
		LastRequestTime: now.Add(-110 * time.Millisecond), // The overall activity is idle
		ChainProgress: map[string]StepState{
			"StaleChain": {
				CurrentStep:   1,
				LastMatchTime: now.Add(-120 * time.Millisecond), // The chain step is older than IdleTimeout
			},
		},
	}
	// Add an entry to the TopN stats to verify it gets cleaned up
	provider.topActorsPerChain["SomeChain"] = map[string]*ActorStats{
		actorUseless.String(): {Hits: 1},
	}

	// --- Act ---
	stopChan := make(chan struct{})
	go CleanUpIdleActors(provider, stopChan)
	defer close(stopChan)

	// Wait for the cleanup routine to signal it has completed a pass.
	select {
	case <-provider.testSignals.CleanupDoneSignal:
		// Cleanup finished.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for cleanup routine to complete.")
	}

	// --- Assert ---
	if _, exists := provider.activityStore[actorUseless]; exists {
		t.Error("Expected 'useless' key to be cleaned up by MaxTimeSinceLastHit, but it still exists.")
	}
	if _, exists := provider.activityStore[actorIdle]; exists {
		t.Error("Expected 'idle' key to be cleaned up by IdleTimeout, but it still exists.")
	}
	if _, exists := provider.activityStore[actorStaleChain]; exists {
		t.Error("Expected key with stale chain progress to be cleaned up, but it still exists.")
	}
	if _, exists := provider.activityStore[actorStillUseful]; !exists {
		t.Error("Expected 'still useful' key to remain, but it was cleaned up.")
	}
	if _, exists := provider.activityStore[actorBlocked]; !exists {
		t.Error("Expected 'blocked' key to remain, but it was cleaned up.")
	}

	// Assert TopN cleanup
	if _, exists := provider.topActorsPerChain["SomeChain"][actorUseless.String()]; exists {
		t.Error("Expected actor to be cleaned up from TopN stats, but it still exists.")
	}
}

func TestIsMoreActiveThan(t *testing.T) {
	tests := []struct {
		name     string
		s1       *ActorStats
		s2       *ActorStats
		expected bool
	}{
		{
			name:     "s1 has more hits",
			s1:       &ActorStats{Resets: 1, Completions: 5, Hits: 20},
			s2:       &ActorStats{Resets: 2, Completions: 10, Hits: 10},
			expected: true,
		},
		{
			name:     "s2 has more hits",
			s1:       &ActorStats{Resets: 2, Completions: 5, Hits: 10},
			s2:       &ActorStats{Resets: 1, Completions: 10, Hits: 20},
			expected: false,
		},
		{
			name:     "Equal hits, s1 has more completions",
			s1:       &ActorStats{Resets: 5, Completions: 6, Hits: 20},
			s2:       &ActorStats{Resets: 10, Completions: 5, Hits: 20},
			expected: true,
		},
		{
			name:     "Equal hits, s2 has more completions",
			s1:       &ActorStats{Resets: 10, Completions: 5, Hits: 20},
			s2:       &ActorStats{Resets: 5, Completions: 6, Hits: 20},
			expected: false,
		},
		{
			name:     "Equal hits and completions, s1 has more resets",
			s1:       &ActorStats{Resets: 2, Completions: 5, Hits: 20},
			s2:       &ActorStats{Resets: 1, Completions: 5, Hits: 20},
			expected: true,
		},
		{
			name:     "Equal hits and completions, s2 has more resets",
			s1:       &ActorStats{Resets: 1, Completions: 5, Hits: 20},
			s2:       &ActorStats{Resets: 2, Completions: 5, Hits: 20},
			expected: false,
		},
		{
			name:     "All stats are equal",
			s1:       &ActorStats{Resets: 5, Completions: 5, Hits: 20},
			s2:       &ActorStats{Resets: 5, Completions: 5, Hits: 20},
			expected: false,
		},
		{
			name:     "s1 is nil",
			s1:       nil,
			s2:       &ActorStats{Resets: 1, Completions: 1, Hits: 10},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s1.IsMoreActiveThan(tt.s2); got != tt.expected {
				t.Errorf("IsMoreActiveThan() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestCleanUpIdleActors_ImmediateShutdown verifies that the cleanup goroutine
// exits immediately if a stop signal is received before the first tick.
func TestCleanUpIdleActors_ImmediateShutdown(t *testing.T) {
	// 1. Setup
	provider := newMockStoreProvider(1*time.Second, 1*time.Second)

	stopChan := make(chan struct{})
	doneChan := make(chan struct{})

	// --- Act ---
	go func() {
		CleanUpIdleActors(provider, stopChan)
		close(doneChan) // Signal that the goroutine has exited.
	}()

	close(stopChan) // Immediately send the stop signal.

	// --- Assert ---
	select {
	case <-doneChan:
		// Success, goroutine exited.
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Timed out waiting for cleanup goroutine to shut down.")
	}
}

func TestActorStringAndParsing(t *testing.T) {
	tests := []struct {
		name     string
		actor    Actor
		expected string
	}{
		{
			name:     "IP only",
			actor:    Actor{IPInfo: utils.NewIPInfo("192.0.2.1")},
			expected: "192.0.2.1",
		},
		{
			name:     "IP with UA",
			actor:    Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), UA: "Mozilla/5.0"},
			expected: "192.0.2.1 | Mozilla/5.0",
		},
		{
			name:     "IP with VHost",
			actor:    Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), VHost: "example.com"},
			expected: "192.0.2.1@example.com",
		},
		{
			name:     "IP with VHost and UA",
			actor:    Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), VHost: "example.com", UA: "Mozilla/5.0"},
			expected: "192.0.2.1@example.com | Mozilla/5.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test String()
			result := tt.actor.String()
			if result != tt.expected {
				t.Errorf("String() = %q, want %q", result, tt.expected)
			}

			// Test round-trip parsing
			parsed, err := NewActorFromString(result)
			if err != nil {
				t.Fatalf("NewActorFromString(%q) error: %v", result, err)
			}
			if parsed.IPInfo.Address != tt.actor.IPInfo.Address {
				t.Errorf("Parsed IP = %q, want %q", parsed.IPInfo.Address, tt.actor.IPInfo.Address)
			}
			if parsed.VHost != tt.actor.VHost {
				t.Errorf("Parsed VHost = %q, want %q", parsed.VHost, tt.actor.VHost)
			}
			if parsed.UA != tt.actor.UA {
				t.Errorf("Parsed UA = %q, want %q", parsed.UA, tt.actor.UA)
			}
		})
	}
}
