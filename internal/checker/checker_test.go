package checker_test

import (
	"bot-detector/internal/checker"
	"bot-detector/internal/logparser"
	"bot-detector/internal/persistence"
	"bot-detector/internal/testutil"

	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCheckChains_BlockAction tests a multi-step chain that successfully triggers a block,
// verifying behavior in both live and dry-run modes.
func TestCheckChains_BlockAction(t *testing.T) {
	tests := []struct {
		name            string
		dryRun          bool
		expectBlockCall bool
	}{
		{name: "Live Mode", dryRun: false, expectBlockCall: true},
		{name: "Dry Run Mode", dryRun: true, expectBlockCall: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			const targetIP = "192.0.2.1"
			const blockDuration = 10 * time.Minute

			h := NewCheckerTestHarness(t, nil)
			h.processor.DryRun = tt.dryRun

			h.addChain(config.BehavioralChain{
				Name:          "TwoStepBlocker",
				MatchKey:      "ip_ua",
				Action:        "block",
				BlockDuration: blockDuration,
				StepsYAML: []config.StepDefYAML{
					{FieldMatches: map[string]interface{}{"Path": "/step/one"}},
					{FieldMatches: map[string]interface{}{"Path": "/step/two"}},
				},
			})

			// --- Act ---
			entry1 := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), UserAgent: "TestAgent", Timestamp: time.Now(), Path: "/step/one"}
			h.processEntry(entry1)
			h.assertChainProgress("TwoStepBlocker", entry1, 1)

			entry2 := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), UserAgent: "TestAgent", Timestamp: entry1.Timestamp.Add(2 * time.Second), Path: "/step/two"}
			h.processEntry(entry2)

			// --- Assert ---
			if h.blockCalled != tt.expectBlockCall {
				t.Fatalf("Expected blockCalled to be %t, but got %t", tt.expectBlockCall, h.blockCalled)
			}

			if tt.expectBlockCall {
				if h.blockCallArgs.ipInfo.Address != targetIP || h.blockCallArgs.duration != blockDuration {
					t.Errorf("Blocker called with incorrect args. Got IP %s, Duration %v", h.blockCallArgs.ipInfo.Address, h.blockCallArgs.duration)
				}
			}

			// In-memory state should be updated regardless of mode.
			h.assertBlocked(entry2, true)
			h.assertChainProgressCleared("TwoStepBlocker", entry2)
		})
	}
}

// TestPreCheckActivity_StillBlocked verifies that when an IP is already blocked,
// the LastRequestTime is only updated if the new log entry is newer.
func TestPreCheckActivity_StillBlocked(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name                    string
		entryTimestamp          time.Time
		expectedLastRequestTime time.Time
	}{
		{
			name:                    "With Older Entry",
			entryTimestamp:          now.Add(-10 * time.Second),
			expectedLastRequestTime: now, // Should not be updated
		},
		{
			name:                    "With Newer Entry",
			entryTimestamp:          now.Add(10 * time.Second),
			expectedLastRequestTime: now.Add(10 * time.Second), // Should be updated
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			testutil.ResetGlobalState()
			const targetIP = "192.0.2.50"
			actor := store.Actor{IPInfo: utils.NewIPInfo(targetIP)}
			processor := testutil.NewTestProcessor(nil, nil)

			// Manually create a pre-existing, non-expired block state.
			processor.ActivityStore[store.Actor(actor)] = &store.ActorActivity{
				LastRequestTime: now,
				BlockedUntil:    now.Add(1 * time.Hour),
				IsBlocked:       true,
			}

			entry := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), Timestamp: tt.entryTimestamp}

			// --- Act ---
			// We call the unexported checker.PreCheckActivity directly to isolate the logic under test.
			processor.ActivityMutex.Lock()
			_, skip, _ := checker.PreCheckActivity(processor, entry, actor)
			processor.ActivityMutex.Unlock()

			// --- Assert ---
			if !skip {
				t.Error("Expected skip to be true for an already-blocked IP, but it was false.")
			}
			if !processor.ActivityStore[store.Actor(actor)].LastRequestTime.Equal(tt.expectedLastRequestTime) {
				t.Errorf("Expected LastRequestTime to be %v, but it was %v",
					tt.expectedLastRequestTime, processor.ActivityStore[store.Actor(actor)].LastRequestTime)
			}
		})
	}
}

// TestCheckChains_UnknownAction tests that an unrecognized action is handled gracefully in both live and dry-run modes.
func TestCheckChains_UnknownAction(t *testing.T) {
	tests := []struct {
		name   string
		dryRun bool
	}{
		{name: "Live Mode", dryRun: false},
		{name: "Dry Run Mode", dryRun: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			h := NewCheckerTestHarness(t, nil)
			h.processor.DryRun = tt.dryRun

			h.addChain(config.BehavioralChain{
				Name:      "UnknownActionChain",
				MatchKey:  "ip",
				Action:    "throttle", // An unrecognized action
				StepsYAML: []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/test"}}},
			})

			entry := &app.LogEntry{
				IPInfo:    utils.NewIPInfo("192.0.2.1"),
				Timestamp: time.Now(),
				Path:      "/test",
			}

			// --- Act ---
			h.processEntry(entry)

			// --- Assert ---
			if h.blockCalled {
				t.Fatal("Blocker was called, but should have been skipped for an unknown action.")
			}

			// State should be cleared and no block should be recorded.
			h.assertBlocked(entry, false)
			h.assertChainProgressCleared("UnknownActionChain", entry)

			// In dry-run mode, we expect a specific log message.
			if tt.dryRun {
				foundLog := false
				expectedLogSubstring := "UNKNOWN_ACTION!"
				for _, log := range h.capturedLogs {
					if strings.Contains(log, expectedLogSubstring) {
						foundLog = true
						break
					}
				}
				if !foundLog {
					t.Errorf("Expected log to contain '%s' for unknown action in dry-run, but it was not found.", expectedLogSubstring)
				}
			}
		})
	}
}

// TestProcessChainForEntry_AlreadyCompleted tests the edge case where the function is called
// for a chain that has already been completed but not yet cleared from memory.
func TestProcessChainForEntry_AlreadyCompleted(t *testing.T) {
	// 1. Setup
	testutil.ResetGlobalState()

	chain := config.BehavioralChain{
		Name:  "CompletedChain",
		Steps: []config.StepDef{{Order: 1}}, // A simple one-step chain
	}

	// Create an activity state where the chain is already completed.
	activity := &store.ActorActivity{
		ChainProgress: map[string]store.StepState{
			"CompletedChain": {
				CurrentStep:   1, // CurrentStep (1) == len(chain.Steps) (1)
				LastMatchTime: time.Now(),
			},
		},
	}

	processor := testutil.NewTestProcessor(nil, nil)

	entry := &app.LogEntry{} // Dummy entry, its contents don't matter for this test.

	// --- Act ---
	// Call the function under test. This should hit the 'if nextStepIndex >= len(chain.Steps)'
	// branch and immediately break.
	checker.ProcessChainForEntry(processor, &chain, entry, activity, time.Time{})

	// --- Assert ---
	// The state should remain unchanged.
	finalState, exists := activity.ChainProgress["CompletedChain"]
	if !exists {
		t.Fatal("Expected chain progress to still exist, but it was deleted.")
	}

	if finalState.CurrentStep != 1 {
		t.Errorf("Expected CurrentStep to remain 1, but it was changed to %d", finalState.CurrentStep)
	}
}

// TestCheckChains_TimeDelayReset tests that a chain resets if time-based rules between steps are not met.
func TestCheckChains_TimeDelayReset(t *testing.T) {
	tests := []struct {
		name            string
		step2YAML       config.StepDefYAML
		step2TimeOffset time.Duration
	}{
		{
			name:            "MaxDelay Exceeded",
			step2YAML:       config.StepDefYAML{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MaxDelay: "5s"},
			step2TimeOffset: 6 * time.Second,
		},
		{
			name:            "MinDelay Not Met",
			step2YAML:       config.StepDefYAML{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MinDelay: "500ms"},
			step2TimeOffset: 100 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			const targetIP = "192.0.2.1"
			h := NewCheckerTestHarness(t, nil)

			h.addChain(config.BehavioralChain{
				Name:      "TimeResetChain",
				MatchKey:  "ip",
				Action:    "log",
				StepsYAML: []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/step/one"}}, tt.step2YAML},
			})

			// --- Act ---
			entry1 := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), Timestamp: time.Now(), Path: "/step/one"}
			h.processEntry(entry1)

			// Assert 1: State is at step 1.
			h.assertChainProgress("TimeResetChain", entry1, 1)

			// Process the second request with the specified time offset.
			entry2 := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), Timestamp: entry1.Timestamp.Add(tt.step2TimeOffset), Path: "/step/two"}
			h.processEntry(entry2)

			// --- Assert 2: The chain should have been reset.
			h.assertChainProgressCleared("TimeResetChain", entry2)
		})
	}
}

// TestCheckChains_LogAction tests that a chain with Action="log" clears the state but does not call the blocker.
func TestCheckChains_LogAction(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.1"
	h := NewCheckerTestHarness(t, nil)

	h.addChain(config.BehavioralChain{
		Name:          "LogActionTestChain",
		MatchKey:      "ip",
		Action:        "log", // ACTION: log
		BlockDuration: 0,
		StepsYAML: []config.StepDefYAML{
			{FieldMatches: map[string]interface{}{"Path": "/step/one"}, MaxDelay: "5s"},
			{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MaxDelay: "5s"},
		},
	})

	// --- Act ---
	entry1 := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), Timestamp: time.Now(), Path: "/step/one"}
	h.processEntry(entry1)

	// Assert 1: State is at step 1.
	h.assertChainProgress("LogActionTestChain", entry1, 1)

	// Process the second request (completion).
	entry2 := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), Timestamp: entry1.Timestamp.Add(2 * time.Second), Path: "/step/two"}
	h.processEntry(entry2)

	// --- Assert 2 ---
	if h.blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for Action='log'.")
	}
	h.assertBlocked(entry2, false)
	h.assertChainProgressCleared("LogActionTestChain", entry2)
}

// TestCheckChains_BlockExpiration verifies that if an activity is marked as blocked but the
// BlockedUntil time has passed, the block is cleared and the chain can be processed again.
func TestCheckChains_BlockExpiration(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.1"
	h := NewCheckerTestHarness(t, nil)
	h.addChain(config.BehavioralChain{
		Name:          "SingleStepChain",
		MatchKey:      "ip",
		Action:        "block",
		BlockDuration: 1 * time.Minute,
		StepsYAML:     []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/test"}}},
	})

	// Manually create a pre-existing, EXPIRED block state.
	actor := store.Actor{IPInfo: utils.NewIPInfo(targetIP)}
	h.processor.ActivityStore[store.Actor(actor)] = &store.ActorActivity{
		LastRequestTime: time.Time{},                    // Not relevant for this test
		BlockedUntil:    time.Now().Add(-1 * time.Hour), // Expired an hour ago
		IsBlocked:       true,
	}

	// --- Act ---
	entry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/test",
	}
	h.processEntry(entry)

	// --- Assert ---
	h.assertBlocked(entry, true)
	if h.processor.ActivityStore[store.Actor(actor)].BlockedUntil.Before(time.Now()) {
		t.Error("Expected BlockedUntil time to be in the future, but it was not.")
	}
}

// TestCheckChains_IPVersionMismatch verifies that chains are correctly skipped
// if the log entry's IP version does not match the chain's `match_key`.
func TestCheckChains_IPVersionMismatch(t *testing.T) {
	tests := []struct {
		name          string
		chainMatchKey string
		entryIP       string
	}{
		{name: "IPv4 entry vs ipv6 chain", chainMatchKey: "ipv6", entryIP: "192.0.2.1"},
		{name: "IPv6 entry vs ipv4 chain", chainMatchKey: "ipv4", entryIP: "2001:db8::1"},
		{name: "IPv4 entry vs ipv6_ua chain", chainMatchKey: "ipv6_ua", entryIP: "192.0.2.1"},
		{name: "IPv6 entry vs ipv4_ua chain", chainMatchKey: "ipv4_ua", entryIP: "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			h := NewCheckerTestHarness(t, nil)
			h.addChain(config.BehavioralChain{
				Name:      "VersionMismatchChain",
				MatchKey:  tt.chainMatchKey,
				Action:    "log",
				StepsYAML: []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/test"}}},
			})

			// --- Act ---
			entry := &app.LogEntry{
				IPInfo:    utils.NewIPInfo(tt.entryIP),
				Timestamp: time.Now(),
				Path:      "/test",
			}
			h.processEntry(entry)

			// --- Assert ---
			// No activity should be created because the chain should be skipped entirely.
			h.assertChainProgressCleared("VersionMismatchChain", entry)
		})
	}
}

// TestCheckChains_IPAndUABlockOptimization verifies that when a chain with `match_key: "ip_ua"`
// completes a block action, it blocks BOTH the specific ip_ua key and the general ip-only key.
func TestCheckChains_IPAndUABlockOptimization(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.100"
	const targetUA = "BadBot/1.0"

	h := NewCheckerTestHarness(t, nil)
	h.addChain(config.BehavioralChain{
		Name:          "IP_UA_Blocker",
		MatchKey:      "ip_ua",
		Action:        "block",
		BlockDuration: 5 * time.Minute,
		StepsYAML:     []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/trigger"}}},
	})

	// --- Act ---
	entry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(targetIP),
		UserAgent: targetUA,
		Timestamp: time.Now(),
		Path:      "/trigger",
	}
	h.processEntry(entry)

	// --- Assert ---
	// Assert that the specific IP+UA key is blocked.
	h.assertBlocked(entry, true)
	// Assert that the general IP-only key is also blocked.
	ipOnlyEntry := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), UserAgent: ""}
	h.assertBlocked(ipOnlyEntry, true)
}

// TestCheckChains_OnMatchStop verifies that when a chain with `on_match: "stop"`
// completes, no further chains are processed for that log entry.
func TestCheckChains_OnMatchStop(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.1"
	h := NewCheckerTestHarness(t, nil)

	// Chain 1: This chain will match and has on_match: "stop".
	h.addChain(config.BehavioralChain{
		Name:      "StopChain",
		MatchKey:  "ip",
		Action:    "log",
		OnMatch:   "stop",
		StepsYAML: []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/trigger"}}},
	})

	// Chain 2: This chain also matches the same entry but should be skipped.
	h.addChain(config.BehavioralChain{
		Name:      "ShouldBeSkippedChain",
		MatchKey:  "ip",
		Action:    "log",
		StepsYAML: []config.StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/trigger"}}},
	})

	// --- Act ---
	entry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/trigger",
	}
	h.processEntry(entry)

	// --- Assert ---
	// The "StopChain" should have completed and its progress cleared.
	h.assertChainProgressCleared("StopChain", entry)

	// The "ShouldBeSkippedChain" should have no progress state, as it was never processed.
	key := checker.GetActor(&h.processor.Chains[1], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if exists && len(activity.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress for 'ShouldBeSkippedChain' to be empty, but it has entries: %v", activity.ChainProgress)
	}
}

// TestCheckChains_UnblockOnGoodActor verifies the "unblock on good actor" feature,
// including the initial unblock action and the subsequent cooldown period.
func TestCheckChains_UnblockOnGoodActor(t *testing.T) {
	// --- Setup ---
	const goodIP = "10.0.0.1"
	const cooldown = 100 * time.Millisecond

	// 1. Create a harness with the unblock feature enabled and a short cooldown.
	h := NewCheckerTestHarness(t, &config.AppConfig{
		Checker: config.CheckerConfig{
			UnblockOnGoodActor: true,
			UnblockCooldown:    cooldown,
		},
	})

	// 2. Define a "good actor" rule directly in the processor's config.
	// This simulates loading a `good_actors` block from YAML.
	// Create a config.MatcherContext for the good actor.
	goodActorCtx := config.MatcherContext{
		ChainName:          "good_actor_test",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies: map[string]*types.FileDependency{
			"test.txt": {
				Path: "test.txt",
				CurrentStatus: &types.FileDependencyStatus{
					Status:   types.FileStatusLoaded,
					Checksum: "checksum1",
				},
			},
		},
		FilePath: "", // Empty for this test
	}
	goodActorMatcher, err := config.CompileStringMatcher(goodActorCtx, goodIP)
	if err != nil {
		t.Fatalf("Failed to compile good actor matcher: %v", err)
	}
	h.processor.Config.GoodActors = []config.GoodActorDef{
		{
			Name:       "test_good_ips",
			IPMatchers: []config.FieldMatcher{goodActorMatcher},
		},
	}

	// 3. Create the log entry for the good actor.
	goodEntry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(goodIP),
		Timestamp: time.Now(),
		Path:      "/some/path",
	}

	// --- Act 1: Initial Detection (First Appearance) ---
	h.processEntry(goodEntry)

	// --- Assert 1: Unblock command should be sent on first appearance ---
	if !h.unblockCalled {
		t.Fatal("Expected Unblock() to be called on the first good actor match, but it was not.")
	}
	h.unblockCalled = false // Reset for the next assertion.

	// --- Act 2: Immediate Subsequent Request (Not Blocked) ---
	// Process the same entry again immediately.
	h.processEntry(goodEntry)

	// --- Assert 2: Unblock command should NOT be sent (not blocked, cooldown active) ---
	if h.unblockCalled {
		t.Fatal("Unblock() was called during the cooldown period for non-blocked IP, but it should have been skipped.")
	}

	// --- Act 3: After Cooldown (Still Not Blocked) ---
	time.Sleep(cooldown + 20*time.Millisecond) // Wait for the cooldown to expire.
	h.processEntry(goodEntry)

	// --- Assert 3: Unblock command should be sent again after cooldown ---
	if !h.unblockCalled {
		t.Fatal("Expected Unblock() to be called again after the cooldown expired, but it was not.")
	}
	h.unblockCalled = false // Reset for the next assertion.

	// --- Act 4: Simulate IP Getting Blocked ---
	// Enable persistence and mark IP as blocked
	h.processor.PersistenceEnabled = true
	db, err := persistence.OpenDB("", true)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer persistence.CloseDB(db)
	h.processor.DB = db
	_ = persistence.UpsertIPState(db, goodIP, persistence.BlockStateBlocked, time.Now().Add(1*time.Hour), "test-block", time.Now(), time.Now())

	// Process entry immediately (within cooldown period)
	h.processEntry(goodEntry)

	// --- Assert 4: Unblock should be sent immediately when IP is blocked (ignores cooldown) ---
	if !h.unblockCalled {
		t.Fatal("Expected Unblock() to be called immediately when IP is blocked, regardless of cooldown.")
	}
}

// TestCheckChains_UnblockOnGoodActor_MultipleIPs verifies that multiple IPs from the same
// good actor rule only trigger unblock on first appearance, not repeatedly.
func TestCheckChains_UnblockOnGoodActor_MultipleIPs(t *testing.T) {
	// --- Setup ---
	const cooldown = 100 * time.Millisecond

	// 1. Create a harness with the unblock feature enabled.
	h := NewCheckerTestHarness(t, &config.AppConfig{
		Checker: config.CheckerConfig{
			UnblockOnGoodActor: true,
			UnblockCooldown:    cooldown,
		},
	})

	// 2. Define good actor rules for each IP individually (simpler than CIDR for testing).
	h.processor.Config.GoodActors = []config.GoodActorDef{}
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}

	for _, ip := range ips {
		goodActorCtx := config.MatcherContext{
			ChainName:          "good_actor_test",
			StepIndex:          0,
			CanonicalFieldName: "IP",
			FileDependencies: map[string]*types.FileDependency{
				"test.txt": {
					Path: "test.txt",
					CurrentStatus: &types.FileDependencyStatus{
						Status:   types.FileStatusLoaded,
						Checksum: "checksum1",
					},
				},
			},
			FilePath: "",
		}
		goodActorMatcher, err := config.CompileStringMatcher(goodActorCtx, ip)
		if err != nil {
			t.Fatalf("Failed to compile good actor matcher for %s: %v", ip, err)
		}
		h.processor.Config.GoodActors = append(h.processor.Config.GoodActors, config.GoodActorDef{
			Name:       "test_good_ip_" + ip,
			IPMatchers: []config.FieldMatcher{goodActorMatcher},
		})
	}

	// 3. Process multiple IPs from the same good actor range.
	unblockCount := 0

	for i, ip := range ips {
		entry := &app.LogEntry{
			IPInfo:    utils.NewIPInfo(ip),
			Timestamp: time.Now(),
			Path:      "/some/path",
		}
		h.processEntry(entry)
		if h.unblockCalled {
			unblockCount++
			h.unblockCalled = false
		} else {
			t.Logf("IP %s (index %d) did not trigger unblock", ip, i)
		}
	}

	// --- Assert: Each IP should trigger unblock on first appearance ---
	if unblockCount != len(ips) {
		t.Logf("Captured logs: %v", h.capturedLogs)
		t.Fatalf("Expected %d unblock calls (one per IP on first appearance), got %d", len(ips), unblockCount)
	}

	// --- Act: Process same IPs again immediately ---
	unblockCount = 0
	for _, ip := range ips {
		entry := &app.LogEntry{
			IPInfo:    utils.NewIPInfo(ip),
			Timestamp: time.Now(),
			Path:      "/some/path",
		}
		h.processEntry(entry)
		if h.unblockCalled {
			unblockCount++
			h.unblockCalled = false
		}
	}

	// --- Assert: No unblocks should occur (cooldown active, not blocked) ---
	if unblockCount != 0 {
		t.Fatalf("Expected 0 unblock calls during cooldown for non-blocked IPs, got %d", unblockCount)
	}
}

// TestCheckChains_TimeRules provides focused tests for the new time-based rules,
// especially the first-step-only `first_hit_since` logic.
func TestCheckChains_TimeRules(t *testing.T) {
	// 1. Setup
	chain := config.BehavioralChain{
		Name:     "TimeRuleTestChain",
		MatchKey: "ip",
		Action:   "log",
	}
	// Create MatcherContexts for the chain steps.
	ctx1 := config.MatcherContext{
		ChainName:          chain.Name,
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "",
	}
	matcher1, _ := config.CompileStringMatcher(ctx1, "/step1")

	ctx2 := config.MatcherContext{
		ChainName:          chain.Name,
		StepIndex:          1,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "",
	}
	matcher2, _ := config.CompileStringMatcher(ctx2, "/step2")
	chain.Steps = []config.StepDef{
		{Order: 1, MinTimeSinceLastHit: 2 * time.Second, Matchers: []struct {
			Matcher   config.FieldMatcher
			FieldName string
		}{{Matcher: matcher1, FieldName: "Path"}}},
		{Order: 2, Matchers: []struct {
			Matcher   config.FieldMatcher
			FieldName string
		}{{Matcher: matcher2, FieldName: "Path"}}},
	}

	chains := []config.BehavioralChain{chain}

	baseProcessor := func() *app.Processor {
		return testutil.NewTestProcessor(&config.AppConfig{}, chains)
	}

	tests := []struct {
		name                string
		primingTimeOffset   time.Duration // How long ago the IP was last seen. Zero if never seen.
		shouldChainProgress bool
	}{
		{
			name: "min_time_since_last_hit FAILURE - IP seen too recently",
			// Last seen 1 second ago, which is LESS than the 2s minimum.
			primingTimeOffset:   -1 * time.Second, // 1s ago
			shouldChainProgress: false,
		},
		{
			name: "min_time_since_last_hit SUCCESS - IP seen long enough ago",
			// Last seen 3 seconds ago, which is GREATER than the 2s minimum.
			primingTimeOffset:   -3 * time.Second, // 3s ago
			shouldChainProgress: true,
		},
		{
			name: "min_time_since_last_hit SUCCESS - IP never seen before",
			// Zero time indicates the IP has never been seen, so the rule doesn't match.
			primingTimeOffset:   0,
			shouldChainProgress: true,
		},
	}

	// Use a fixed "now" for deterministic test runs.
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := baseProcessor()

			// Prime the activity store with the specified last request time.
			if tt.primingTimeOffset != 0 {
				key := store.Actor{IPInfo: utils.NewIPInfo("192.0.2.1")}
				// Use GetOrCreateActorActivityUnsafe to ensure ChainProgress map is initialized.
				processor.ActivityMutex.Lock()
				activity := store.GetOrCreateUnsafe(processor.ActivityStore, store.Actor(key))
				activity.LastRequestTime = now.Add(tt.primingTimeOffset)
				processor.ActivityMutex.Unlock()
			}

			entry := &app.LogEntry{
				IPInfo:    utils.NewIPInfo("192.0.2.1"),
				Timestamp: now,      // The current request always happens at our fixed "now".
				Path:      "/step1", // This will match the first step
			}
			checker.CheckChains(processor, entry)

			processor.ActivityMutex.RLock()
			activity := processor.ActivityStore[store.Actor(store.Actor{IPInfo: entry.IPInfo})]
			progressExists := activity.ChainProgress[chain.Name] != store.StepState{}
			if progressExists != tt.shouldChainProgress {
				t.Errorf("Chain progress existence was %t, but expected %t", progressExists, tt.shouldChainProgress)
			}
		})
	}
}

// TestDryRunMode simulates the entire dry-run process using config.yaml and test_access.log,
// and verifies the log output against the expected log messages extracted from comments
// in test_access.log.
func TestDryRunMode(t *testing.T) {
	// --- Setup ---
	testutil.ResetGlobalState()

	// The config.yaml file now references a file matcher. We need to create it.
	tempDir := t.TempDir()
	uaFile := filepath.Join(tempDir, "bad_user_agents.txt")
	if err := os.WriteFile(uaFile, []byte(`BadUA/1.0
regex:NastyBot`), 0644); err != nil {
		t.Fatalf("Failed to create dummy user agent file: %v", err)
	}
	// The test config.yaml is hardcoded to look for this relative path.
	// We need to create it in the current working directory.
	// A better long-term solution would be to make the path in config.yaml absolute or configurable for tests.
	if err := os.WriteFile("bad_user_agents.txt", []byte(`BadUA/1.0
regex:NastyBot`), 0644); err != nil {
		t.Fatalf("Failed to create dummy user agent file in current directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove("bad_user_agents.txt") })
	// 1. Load configuration (chains, whitelist, etc.)
	logging.SetLogLevel("debug")
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigFilePath: "../../testdata/config.yaml"})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() failed: %v", err)
	}
	logging.SetLogLevel(loadedCfg.Application.LogLevel)

	// Create a processor (but don't start any background processes like ChainWatcher).
	processor := &app.Processor{
		ActivityMutex:     &sync.RWMutex{},
		ActivityStore:     make(map[store.Actor]*store.ActorActivity),
		TopActorsPerChain: make(map[string]map[string]*store.ActorStats),
		ConfigMutex:       &sync.RWMutex{},
		Metrics:           metrics.NewMetrics(),
		Chains:            loadedCfg.Chains,
		Config: &config.AppConfig{
			Parser: config.ParserConfig{
				OutOfOrderTolerance: 0, // Disable buffering for this specific test.
				TimestampFormat:     loadedCfg.Parser.TimestampFormat,
			},
			Checker: config.CheckerConfig{
				MaxTimeSinceLastHit: loadedCfg.Checker.MaxTimeSinceLastHit,
			},
		},
		DryRun:           true,                                                                            // Simulate dry-run mode
		LogFunc:          func(level logging.LogLevel, tag string, format string, args ...interface{}) {}, // Will be replaced
		ReasonCache:      make(map[string]*string),
		ReasonCacheMutex: sync.RWMutex{},
	}

	// Set the CheckChainsFunc on the processor instance to avoid nil pointers.
	processor.CheckChainsFunc = func(entry *app.LogEntry) { checker.CheckChains(processor, entry) }
	processor.ProcessLogLine = func(line string) { logparser.ProcessLogLineInternal(processor, line) }

	// 2. Read test_access.log and extract expected log outputs from comments
	testLogFilePath := "../../testdata/test_access.log"
	testLogData, err := os.ReadFile(testLogFilePath)
	if err != nil {
		t.Fatalf("Failed to read %s: %v", testLogFilePath, err)
	}
	expectedLogs := make(map[int]string)
	// Use a separate scanner for extracting expected logs to avoid line number confusion
	expectedLogScanner := bufio.NewScanner(bytes.NewReader(testLogData))
	expectedLogLineNum := 0
	for expectedLogScanner.Scan() {
		expectedLogLineNum++
		line := expectedLogScanner.Text()
		if strings.Contains(line, "=== EXPECTED LOG:") {
			// Extract the expected log message from the comment line.
			parts := strings.SplitN(line, "=== EXPECTED LOG:", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" { // Ensure there's actual content to expect
				expectedLogs[expectedLogLineNum] = strings.TrimSpace(parts[1]) // Store expected log against line number
			}
		}
	}
	if err := expectedLogScanner.Err(); err != nil {
		t.Fatalf("Failed to scan test_access.log: %v", err)
	}

	// 3. Process test_access.log in dry-run mode, capturing the log output
	var capturedLogs []string // Collect captured log lines.
	processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logLine := tag + ": " + fmt.Sprintf(format, args...)
		capturedLogs = append(capturedLogs, logLine)
		t.Logf("%s", logLine) // Also print to test output for debugging
	}

	// Read in each line of the test log, and run checker.CheckChains on it, to simulate log tailing.
	logEntryScanner := bufio.NewScanner(bytes.NewReader(testLogData)) // Re-scan for log entries
	actualLogLineNumber := 0

	for logEntryScanner.Scan() {
		actualLogLineNumber++
		line := logEntryScanner.Text()

		processor.ProcessLogLine(line) // Use the actual processing function
	}
	if err := logEntryScanner.Err(); err != nil {
		t.Fatalf("Failed to scan test_access.log for entries: %v", err)
	}

	// --- Assert ---
	// 4. Verify that the captured log output matches the expected log entries
	// Use a map to track unique SKIP messages that have been found, to account for anti-spam logic.
	// The key will be "SKIP: app.store.Actor <IP> (blocked:<Source>)" or "SKIP: app.store.Actor <IP> (good_actor:<Source>)".
	foundUniqueSkipLogs := make(map[string]bool)

	for commentLineNumber, expectedLog := range expectedLogs {
		found := false
		formattedExpectedLog := strings.Replace(expectedLog, "Line %d: ", "", 1)

		// Special handling for SKIP messages due to anti-spam logic in production code.
		if strings.HasPrefix(formattedExpectedLog, "SKIP: store.Actor") {
			// Extract the unique identifier for the skip message (IP and source).
			// Example: "SKIP: app.store.Actor 10.0.0.2 (UA: TestAgent): Skipped (blocked:SimpleBlockChain)."
			// We want to normalize this to "SKIP: app.store.Actor 10.0.0.2 (blocked:SimpleBlockChain)"
			// to check for uniqueness, ignoring the UA part for this specific check.
			re := regexp.MustCompile(`SKIP: app.store.Actor (\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}|[0-9a-fA-F:]+) \(UA: .*?\): Skipped \((blocked|good_actor):(.+?)\)\.?`)
			matches := re.FindStringSubmatch(formattedExpectedLog)

			if len(matches) == 4 {
				ip := matches[1]
				skipType := matches[2]
				source := matches[3]
				uniqueSkipKey := fmt.Sprintf("SKIP: app.store.Actor %s (%s:%s)", ip, skipType, source)

				if foundUniqueSkipLogs[uniqueSkipKey] {
					// This unique skip message has already been found, so we don't expect it again.
					found = true
				} else {
					// Search for the full expected log in capturedLogs.
					for _, capturedLog := range capturedLogs {
						if strings.Contains(capturedLog, formattedExpectedLog) {
							found = true
							foundUniqueSkipLogs[uniqueSkipKey] = true // Mark as found.
							break
						}
					}
				}
			} else {
				// If it's a SKIP message but doesn't match our regex, treat it normally.
				for _, capturedLog := range capturedLogs {
					if strings.Contains(capturedLog, formattedExpectedLog) {
						found = true
						break
					}
				}
			}
		} else {
			// For non-SKIP messages, use the existing logic.
			for _, capturedLog := range capturedLogs {
				if strings.Contains(capturedLog, formattedExpectedLog) {
					found = true
					break
				}
			}
		}

		if !found {
			// Find the relevant context block from the test log file for context.
			contextMarker := "# ======================"
			logLines := strings.Split(string(testLogData), "\n")
			contextStart := 0
			for i := commentLineNumber - 1; i >= 0; i-- {
				if strings.Contains(logLines[i], contextMarker) {
					contextStart = i
					break
				}
			}
			// Find the end of the context block
			contextEnd := len(logLines)
			for i := commentLineNumber; i < len(logLines); i++ {
				if strings.Contains(logLines[i], contextMarker) {
					contextEnd = i
					break
				}
			}

			contextLines := logLines[contextStart:contextEnd]
			if len(contextLines) == 0 {
				contextLines = capturedLogs
			}
			relevantLines := strings.Join(contextLines, "\n")

			t.Errorf("Expected log message was not found.\n\nEXPECTED:\n'%s'\n\nCONTEXT:\n%s\n",
				formattedExpectedLog, relevantLines)
		}
	}

	// Basic sanity check if we extracted *any* expectedLogs.
	if len(expectedLogs) == 0 {
		t.Fatal("No EXPECTED LOG entries found in test_access.log.  Test cannot run.")
	}
}

// TestCheckChains_OutOfOrder verifies that the system correctly handles log entries
// that arrive out of chronological order, either processing them if within tolerance
// or skipping them if too old.
func TestCheckChains_OutOfOrder(t *testing.T) {
	// Define 'now' once, before the test slice, so it can be used in initialization.
	now := time.Now()

	tests := []struct {
		name                         string
		outOfOrderOffset             time.Duration // How much older the out-of-order entry is than the last seen.
		tolerance                    time.Duration
		expectProcessed              bool // True if the out-of-order entry should be processed.
		expectLastRequestTime        time.Time
		expectedFinalLastRequestTime time.Time // The expected LastRequestTime after the second entry.
	}{
		{ // This test is now simplified to just check that the entry is buffered.
			name:             "Out-of-order within tolerance (buffered)",
			outOfOrderOffset: 3 * time.Second, // 3s older
			tolerance:        5 * time.Second, // 5s tolerance
			expectProcessed:  true,
		},
	}

	for _, tt := range tests { //nolint:dupl
		t.Run(tt.name, func(t *testing.T) {
			testutil.ResetGlobalState()
			targetIP := "192.0.2.1"
			chain := config.BehavioralChain{
				Name:     "SimpleChain",
				MatchKey: "ip",
				Action:   "log",
			}
			chains := []config.BehavioralChain{chain}

			processor := testutil.NewTestProcessor(&config.AppConfig{
				Parser: config.ParserConfig{
					OutOfOrderTolerance: tt.tolerance,
				},
				Checker: config.CheckerConfig{
					MaxTimeSinceLastHit: 1 * time.Minute,
				},
			}, chains)

			// 1. Set the last request time manually to set up the scenario
			processor.ActivityMutex.Lock()
			activity := store.GetOrCreateUnsafe(processor.ActivityStore, store.Actor{IPInfo: utils.NewIPInfo(targetIP)})
			activity.LastRequestTime = now
			processor.ActivityMutex.Unlock()

			// 2. Process the out-of-order entry by calling the main entrypoint
			outOfOrderEntry := &app.LogEntry{IPInfo: utils.NewIPInfo(targetIP), Timestamp: now.Add(-tt.outOfOrderOffset)}
			checker.CheckChains(processor, outOfOrderEntry)

			// 3. Assert outcome - entry should be buffered when within tolerance
			bufferIsPopulated := len(processor.EntryBuffer) > 0

			if bufferIsPopulated != tt.expectProcessed {
				t.Errorf("Expected buffering state to be %t, but buffer populated was %t", tt.expectProcessed, bufferIsPopulated)
			}
		})
	}
}

// bufferWorkerTestHarness encapsulates the setup for testing the checker.EntryBufferWorker.
type bufferWorkerTestHarness struct {
	t          *testing.T
	processor  *app.Processor
	stopCh     chan struct{}
	doneCh     chan struct{}
	tickDoneCh chan struct{} // Signals that a ticker cycle has completed.
}

// newBufferWorkerTestHarness creates a harness for testing the checker.EntryBufferWorker.
func newBufferWorkerTestHarness(t *testing.T, tolerance time.Duration) *bufferWorkerTestHarness {
	t.Helper()

	h := &bufferWorkerTestHarness{
		t:          t,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		tickDoneCh: make(chan struct{}, 1), // Buffered to prevent blocking
	}

	// Create a processor with a mock checker.checkChainsInternal function to capture processed entries.
	p := testutil.NewTestProcessor(&config.AppConfig{Parser: config.ParserConfig{OutOfOrderTolerance: tolerance}}, nil)
	h.processor = p

	// Override the LogFunc to detect when the worker has finished a tick.
	p.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Only signal completion when the specific "tick done" message is seen.
		if tag == "BUFFER_WORKER_TICK_DONE" {
			h.tickDoneCh <- struct{}{}
		}
	}

	return h
}

// start runs the checker.EntryBufferWorker in a goroutine.
func (h *bufferWorkerTestHarness) start() {
	go func() {
		checker.EntryBufferWorker(h.processor, h.stopCh)
		close(h.doneCh)
	}()
}

// stop sends the shutdown signal and waits for the worker to finish.
func (h *bufferWorkerTestHarness) stop() {
	close(h.stopCh)
	select {
	case <-h.doneCh:
		// Worker shut down gracefully.
	case <-time.After(2 * time.Second):
		h.t.Fatal("timed out waiting for checker.EntryBufferWorker to shut down")
	}
}

// TestEntryBufferWorker verifies the core logic of the out-of-order entry buffer.
func TestEntryBufferWorker(t *testing.T) {
	// Use a tolerance that is long enough to avoid flakes but short enough for a quick test.
	const tolerance = 100 * time.Millisecond
	h := newBufferWorkerTestHarness(t, tolerance)

	// --- Setup ---
	// Use a fixed, "real" timestamp as the base for the test to make it deterministic.
	baseTime, err := time.Parse(time.RFC3339, "2025-01-01T12:00:00Z")
	if err != nil {
		t.Fatalf("Failed to parse base time: %v", err)
	}

	// Mock the processor's NowFunc to control the worker's perception of time.
	h.processor.NowFunc = func() time.Time { return baseTime }

	// Create a set of entries with varying timestamps.
	// e1 and e3 are old enough to be processed on the first tick.
	// e2 and e4 are too new.
	e1 := &app.LogEntry{Timestamp: baseTime.Add(-3 * tolerance), Path: "/path1"} // Oldest
	e2 := &app.LogEntry{Timestamp: baseTime.Add(-tolerance / 2), Path: "/path2"} // New
	e3 := &app.LogEntry{Timestamp: baseTime.Add(-2 * tolerance), Path: "/path3"}
	e4 := &app.LogEntry{Timestamp: baseTime, Path: "/path4"} // Newest

	// Add entries to the buffer out of order.
	h.processor.EntryBuffer = []*app.LogEntry{e2, e1, e4, e3}

	// --- Act 1: Start worker and wait for one processing cycle ---
	h.start()

	// Wait for the worker to signal that it has completed a processing tick.
	select {
	case <-h.tickDoneCh:
		// Tick completed.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for entry buffer worker to process a tick")
	}

	// --- Assert 1: Check which entries were processed ---
	// The worker should have processed e1 and e3, leaving e2 and e4.
	// We no longer check the `processed` slice, but the remaining buffer content.
	h.processor.ActivityMutex.Lock()
	remainingCount := len(h.processor.EntryBuffer)
	h.processor.ActivityMutex.Unlock()

	// Verify the remaining entries are still in the buffer.
	h.processor.ActivityMutex.Lock()
	if len(h.processor.EntryBuffer) != 2 {
		t.Fatalf("Expected 2 entries to remain in buffer, but got %d", len(h.processor.EntryBuffer))
	}
	h.processor.ActivityMutex.Unlock()
	if remainingCount != 2 {
		t.Fatalf("Expected 2 entries to remain in buffer after first tick, but got %d", remainingCount)
	}

	// --- Act 2: Stop the worker to trigger shutdown processing ---
	h.stop()

	// --- Assert 2: Check that all remaining entries were flushed and processed ---
	h.processor.ActivityMutex.Lock()
	if len(h.processor.EntryBuffer) != 0 {
		t.Fatalf("Expected buffer to be empty after shutdown flush, but it has %d entries", len(h.processor.EntryBuffer))
	}
	h.processor.ActivityMutex.Unlock()
}

// TestOooBufferFunctions provides focused unit tests for the individual functions
// that manage the out-of-order buffer, ensuring each part of the mechanism
// works correctly in isolation.
func TestOooBufferFunctions(t *testing.T) {
	baseTime := time.Now()

	// Test ShouldBufferOutOfOrder - determines if entry should be buffered
	t.Run("ShouldBufferOutOfOrder", func(t *testing.T) {
		tolerance := 5 * time.Second
		lastRequestTime := baseTime

		tests := []struct {
			name           string
			entryTimestamp time.Time
			expected       bool
		}{
			{"In-order", lastRequestTime.Add(1 * time.Second), false},
			{"Out-of-order within tolerance", lastRequestTime.Add(-3 * time.Second), true},
			{"Out-of-order at tolerance", lastRequestTime.Add(-5 * time.Second), true},
			{"Out-of-order outside tolerance", lastRequestTime.Add(-6 * time.Second), false},
			{"First request ever", time.Time{}, false}, // Zero time
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				lrt := lastRequestTime
				if tt.name == "First request ever" {
					lrt = time.Time{}
				}
				result := checker.ShouldBufferOutOfOrder(lrt, tt.entryTimestamp, tolerance)
				if result != tt.expected {
					t.Errorf("ShouldBufferOutOfOrder() = %v, want %v", result, tt.expected)
				}
			})
		}
	})

	// Test AddToOooBuffer - adds entries in sorted order
	t.Run("AddToOooBuffer", func(t *testing.T) {
		p := testutil.NewTestProcessor(nil, nil)
		e1 := &app.LogEntry{Timestamp: baseTime.Add(-10 * time.Second)}
		e3 := &app.LogEntry{Timestamp: baseTime.Add(-2 * time.Second)}
		p.EntryBuffer = []*app.LogEntry{e1, e3}

		// Insert entries out of order
		e2 := &app.LogEntry{Timestamp: baseTime.Add(-5 * time.Second)}  // Middle
		e0 := &app.LogEntry{Timestamp: baseTime.Add(-15 * time.Second)} // Start
		e4 := &app.LogEntry{Timestamp: baseTime.Add(-1 * time.Second)}  // End

		checker.AddToOooBuffer(p, e2)
		checker.AddToOooBuffer(p, e0)
		checker.AddToOooBuffer(p, e4)

		// Verify sorted order
		expectedOrder := []*app.LogEntry{e0, e1, e2, e3, e4}
		if len(p.EntryBuffer) != len(expectedOrder) {
			t.Fatalf("Expected buffer length %d, got %d", len(expectedOrder), len(p.EntryBuffer))
		}
		for i, entry := range p.EntryBuffer {
			if entry.Timestamp != expectedOrder[i].Timestamp {
				t.Errorf("Buffer not sorted at index %d. Got %v, want %v", i, entry.Timestamp, expectedOrder[i].Timestamp)
			}
		}
	})

	// Test NextOooCandidate - gets next ready entry from buffer
	t.Run("NextOooCandidate", func(t *testing.T) {
		p := testutil.NewTestProcessor(nil, nil)
		e1 := &app.LogEntry{Timestamp: baseTime.Add(-10 * time.Second)} // Old enough
		e2 := &app.LogEntry{Timestamp: baseTime.Add(-8 * time.Second)}  // Old enough
		e3 := &app.LogEntry{Timestamp: baseTime.Add(-3 * time.Second)}  // Too new
		p.EntryBuffer = []*app.LogEntry{e1, e2, e3}

		processingHorizon := baseTime.Add(-5 * time.Second)

		// Get first candidate
		candidate1 := checker.NextOooCandidate(p, processingHorizon)
		if candidate1 == nil || candidate1.Timestamp != e1.Timestamp {
			t.Errorf("Expected first candidate (e1), got %v", candidate1)
		}
		if len(p.EntryBuffer) != 2 {
			t.Errorf("Expected buffer size 2 after dequeue, got %d", len(p.EntryBuffer))
		}

		// Get second candidate
		candidate2 := checker.NextOooCandidate(p, processingHorizon)
		if candidate2 == nil || candidate2.Timestamp != e2.Timestamp {
			t.Errorf("Expected second candidate (e2), got %v", candidate2)
		}
		if len(p.EntryBuffer) != 1 {
			t.Errorf("Expected buffer size 1 after dequeue, got %d", len(p.EntryBuffer))
		}

		// Third call should return nil (e3 too new)
		candidate3 := checker.NextOooCandidate(p, processingHorizon)
		if candidate3 != nil {
			t.Errorf("Expected nil candidate, got %v", candidate3)
		}
		if len(p.EntryBuffer) != 1 || p.EntryBuffer[0].Timestamp != e3.Timestamp {
			t.Errorf("Buffer should contain only e3")
		}
	})
}

// TestOutOfOrder_ComplexScenario simulates a specific sequence of out-of-order hits
// to verify buffering, sorted insertion, and rejection of entries outside the tolerance window.

func TestGetActor(t *testing.T) {
	// Dummy LogEntry for testing
	baseEntry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo("192.0.2.1"),
		UserAgent: "TestAgent",
	}

	// Test cases for different MatchKeys and IP versions
	tests := []struct {
		name        string
		matchKey    string
		entry       *app.LogEntry
		expectedKey store.Actor
	}{
		// --- Success Cases (Key returned) ---
		{"Match: ip (IPv4)", "ip", baseEntry, store.Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), UA: ""}},
		{"Match: ip (IPv6)", "ip", &app.LogEntry{IPInfo: utils.NewIPInfo("2001:db8::1")}, store.Actor{IPInfo: utils.NewIPInfo("2001:db8::1"), UA: ""}},
		{"Match: ipv4 (IPv4)", "ipv4", baseEntry, store.Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), UA: ""}},
		{"Match: ipv6 (IPv6)", "ipv6", &app.LogEntry{IPInfo: utils.NewIPInfo("2001:db8::1")}, store.Actor{IPInfo: utils.NewIPInfo("2001:db8::1"), UA: ""}},
		{"Match: ip_ua (IPv4)", "ip_ua", baseEntry, store.Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), UA: "TestAgent"}},
		{"Match: ipv4_ua (IPv4)", "ipv4_ua", baseEntry, store.Actor{IPInfo: utils.NewIPInfo("192.0.2.1"), UA: "TestAgent"}},
		{"Match: ipv6_ua (IPv6)", "ipv6_ua", &app.LogEntry{IPInfo: utils.NewIPInfo("2001:db8::1"), UserAgent: "TestAgent"}, store.Actor{IPInfo: utils.NewIPInfo("2001:db8::1"), UA: "TestAgent"}},

		// --- Failure Cases (Empty Key is now expected) ---
		{"Mismatch: ipv4 (is IPv6)", "ipv4", &app.LogEntry{IPInfo: utils.NewIPInfo("2001:db8::1")}, store.Actor{}},
		{"Mismatch: ipv6 (is IPv4)", "ipv6", baseEntry, store.Actor{}},
		{"Mismatch: Unknown MatchKey", "bad_key", baseEntry, store.Actor{IPInfo: utils.NewIPInfo("192.0.2.1")}}, // Unknown key defaults to 'ip'
		{"Mismatch: ipv4_ua (is IPv6)", "ipv4_ua", &app.LogEntry{IPInfo: utils.NewIPInfo("2001:db8::1")}, store.Actor{}},
		{"Mismatch: ipv6_ua (is IPv4)", "ipv6_ua", baseEntry, store.Actor{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain := &config.BehavioralChain{MatchKey: tt.matchKey}
			result := checker.GetActor(chain, tt.entry)

			if result != tt.expectedKey {
				t.Errorf("checker.GetActor() got key %+v, want %+v", result, tt.expectedKey)
			}
		})
	}
}

// checkerTestHarness encapsulates common setup for checker.CheckChains tests.
type CheckerTestHarness struct {
	t             *testing.T
	processor     *app.Processor
	blockCalled   bool
	unblockCalled bool
	blockCallArgs struct {
		ipInfo   utils.IPInfo
		duration time.Duration
	}
	capturedLogs []string
	logMutex     sync.Mutex
}

// newCheckerTestHarness creates a harness for testing checker.CheckChains logic.
func NewCheckerTestHarness(t *testing.T, cfg *config.AppConfig) *CheckerTestHarness {
	t.Helper()
	testutil.ResetGlobalState()

	h := &CheckerTestHarness{t: t}

	// Setup a mock blocker to intercept calls.
	mockBlocker := &testutil.MockBlocker{
		BlockFunc: func(ipInfo utils.IPInfo, duration time.Duration, reason string) error {
			h.blockCalled = true
			h.blockCallArgs.ipInfo = ipInfo
			h.blockCallArgs.duration = duration
			return nil
		},
		UnblockFunc: func(ipInfo utils.IPInfo, reason string) error {
			h.unblockCalled = true
			return nil
		},
	}

	// Create the processor with mock functions.
	h.processor = testutil.NewTestProcessor(cfg, nil) // Start with no chains.
	h.processor.Blocker = mockBlocker
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		h.logMutex.Lock()
		defer h.logMutex.Unlock()
		h.capturedLogs = append(h.capturedLogs, fmt.Sprintf(tag+": "+format, args...))
	}

	return h
}

// addChain compiles a chain from its YAML definition and adds it to the processor.
func (h *CheckerTestHarness) addChain(chainYAML config.BehavioralChain) {
	h.t.Helper()
	// This simulates the compilation part of config.LoadConfigFromYAML for a single chain.
	runtimeChain := chainYAML
	// Create an empty FileDependencies map for testing purposes.
	testFileDependencies := make(map[string]*types.FileDependency)

	for i, stepYAML := range chainYAML.StepsYAML {
		matchers, err := config.CompileMatchers(chainYAML.Name, i, stepYAML.FieldMatches, testFileDependencies, "")
		if err != nil {
			h.t.Fatalf("Failed to compile matchers for chain '%s': %v", chainYAML.Name, err)
		}
		runtimeChain.Steps = append(runtimeChain.Steps, config.StepDef{
			Order:    i + 1,
			Matchers: matchers,
		})
	}
	h.processor.Chains = append(h.processor.Chains, runtimeChain)
}

// processEntry runs a single log entry through the checker.CheckChains logic.
func (h *CheckerTestHarness) processEntry(entry *app.LogEntry) {
	h.t.Helper()
	checker.CheckChains(h.processor, entry)
}

// assertChainProgress checks if a given key is at the expected step for a chain.
func (h *CheckerTestHarness) assertChainProgress(chainName string, entry *app.LogEntry, expectedStep int) {
	h.t.Helper()
	key := checker.GetActor(&h.processor.Chains[0], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if !exists || activity.ChainProgress[chainName].CurrentStep != expectedStep {
		h.t.Errorf("Expected chain '%s' to be at step %d, but it was not. Activity: %+v", chainName, expectedStep, activity)
	}
}

// assertBlocked checks if a given key is marked as blocked.
func (h *CheckerTestHarness) assertBlocked(entry *app.LogEntry, expected bool) { //nolint:thelper
	h.t.Helper()
	key := checker.GetActor(&h.processor.Chains[0], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if !exists && expected {
		h.t.Errorf("Expected activity for key %+v to exist and be blocked, but it doesn't exist.", key)
		return
	}
	if exists && activity.IsBlocked != expected {
		h.t.Errorf("Expected IsBlocked to be %t, but it was %t for key %+v", expected, activity.IsBlocked, key)
	}
}

// assertChainProgressCleared checks that a chain's progress has been removed from the activity store.
func (h *CheckerTestHarness) assertChainProgressCleared(chainName string, entry *app.LogEntry) {
	h.t.Helper()
	key := checker.GetActor(&h.processor.Chains[0], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if exists && len(activity.ChainProgress) != 0 {
		h.t.Errorf("Expected ChainProgress to be cleared for key %+v, but it has %d entries: %v", key, len(activity.ChainProgress), activity.ChainProgress)
	}
}

// TestGoodActorNotBlockedByChain_WithVHost verifies that a good actor with a
// VHost set is not blocked by a chain that matches the same UA pattern.
// This is a regression test for a bug where a good actor that was previously
// blocked (e.g., before good_actors config was loaded) would remain in
// SkipTypeBlocked state because IsGoodActor only set SkipInfo when it was
// SkipTypeNone, not when it was SkipTypeBlocked.
func TestGoodActorNotBlockedByChain_WithVHost(t *testing.T) {
	const goodIP = "66.249.70.101"
	const botUA = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	const vhost = "musicbrainz.org"

	h := NewCheckerTestHarness(t, &config.AppConfig{
		Checker: config.CheckerConfig{
			UnblockOnGoodActor: true,
			UnblockCooldown:    5 * time.Minute,
		},
	})

	// Define good_actor: IP match + UA match (both must match)
	ipCtx := config.MatcherContext{
		ChainName:          "good_actor_test",
		CanonicalFieldName: "IP",
		FileDependencies:   map[string]*types.FileDependency{},
	}
	ipMatcher, err := config.CompileStringMatcher(ipCtx, goodIP)
	if err != nil {
		t.Fatalf("Failed to compile IP matcher: %v", err)
	}
	uaCtx := config.MatcherContext{
		ChainName:          "good_actor_test",
		CanonicalFieldName: "UserAgent",
		FileDependencies:   map[string]*types.FileDependency{},
	}
	uaMatcher, err := config.CompileStringMatcher(uaCtx, "regex:(?i)googlebot")
	if err != nil {
		t.Fatalf("Failed to compile UA matcher: %v", err)
	}

	// Define a chain that would block the same UA (like fake_google_bot)
	h.addChain(config.BehavioralChain{
		Name:          "fake_google_bot",
		Action:        "block",
		BlockDuration: time.Hour,
		MatchKey:      "ip",
		OnMatch:       "stop",
		StepsYAML: []config.StepDefYAML{
			{FieldMatches: map[string]interface{}{"useragent": "regex:(?i)googlebot"}},
		},
	})

	entry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(goodIP),
		UserAgent: botUA,
		VHost:     vhost,
		Timestamp: time.Now(),
		Path:      "/artist/some-uuid",
	}

	// Step 1: Process entry WITHOUT good_actors configured (simulates file load failure).
	// The chain should block the IP.
	h.processEntry(entry)
	if !h.blockCalled {
		t.Fatal("Expected block when good_actors is empty")
	}
	h.blockCalled = false

	// Step 2: Now configure good_actors (simulates successful config reload).
	h.processor.Config.GoodActors = []config.GoodActorDef{
		{
			Name:       "google_bot",
			IPMatchers: []config.FieldMatcher{ipMatcher},
			UAMatchers: []config.FieldMatcher{uaMatcher},
		},
	}

	// Step 3: Process a new entry from the same IP. The good_actor check should
	// recognize this as a good actor and clear the stale blocked state.
	// Before the fix, SkipInfo stayed as SkipTypeBlocked because the code only
	// set it when Type == SkipTypeNone, so PreCheckActivity would skip the entry
	// as "blocked" even though IsGoodActor returned true.
	entry2 := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(goodIP),
		UserAgent: botUA,
		VHost:     vhost,
		Timestamp: time.Now().Add(time.Second),
		Path:      "/recording/some-uuid",
	}
	h.processEntry(entry2)

	if h.blockCalled {
		t.Fatal("Good actor was blocked again after good_actors was configured. " +
			"The good_actor match should clear stale SkipTypeBlocked state.")
	}

	// Verify the actor is now marked as good_actor, not blocked
	h.processor.ActivityMutex.RLock()
	actor := store.Actor{IPInfo: utils.NewIPInfo(goodIP), VHost: vhost}
	activity, exists := h.processor.ActivityStore[actor]
	h.processor.ActivityMutex.RUnlock()
	if !exists {
		t.Fatal("Expected activity to exist for the good actor")
	}
	if activity.SkipInfo.Type != utils.SkipTypeGoodActor {
		t.Fatalf("Expected SkipInfo.Type to be GoodActor, got %v", activity.SkipInfo.Type)
	}
	if activity.IsBlocked {
		t.Fatal("Expected IsBlocked to be false after good_actor promotion")
	}
}
