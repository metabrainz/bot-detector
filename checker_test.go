package main

import (
	"bot-detector/internal/logging"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

			h := newCheckerTestHarness(t, nil)
			h.processor.DryRun = tt.dryRun

			h.addChain(BehavioralChain{
				Name:          "TwoStepBlocker",
				MatchKey:      "ip_ua",
				Action:        "block",
				BlockDuration: blockDuration,
				StepsYAML: []StepDefYAML{
					{FieldMatches: map[string]interface{}{"Path": "/step/one"}},
					{FieldMatches: map[string]interface{}{"Path": "/step/two"}},
				},
			})

			// --- Act ---
			entry1 := &LogEntry{IPInfo: NewIPInfo(targetIP), UserAgent: "TestAgent", Timestamp: time.Now(), Path: "/step/one"}
			h.processEntry(entry1)
			h.assertChainProgress("TwoStepBlocker", entry1, 1)

			entry2 := &LogEntry{IPInfo: NewIPInfo(targetIP), UserAgent: "TestAgent", Timestamp: entry1.Timestamp.Add(2 * time.Second), Path: "/step/two"}
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
			resetGlobalState()
			const targetIP = "192.0.2.50"
			actor := Actor{IPInfo: NewIPInfo(targetIP)}
			processor := newTestProcessor(nil, nil)

			// Manually create a pre-existing, non-expired block state.
			processor.ActivityStore[actor] = &ActorActivity{
				LastRequestTime: now,
				BlockedUntil:    now.Add(1 * time.Hour),
				IsBlocked:       true,
			}

			entry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: tt.entryTimestamp}

			// --- Act ---
			// We call the unexported preCheckActivity directly to isolate the logic under test.
			processor.ActivityMutex.Lock()
			_, skip := preCheckActivity(processor, entry, actor)
			processor.ActivityMutex.Unlock()

			// --- Assert ---
			if !skip {
				t.Error("Expected skip to be true for an already-blocked IP, but it was false.")
			}
			if !processor.ActivityStore[actor].LastRequestTime.Equal(tt.expectedLastRequestTime) {
				t.Errorf("Expected LastRequestTime to be %v, but it was %v",
					tt.expectedLastRequestTime, processor.ActivityStore[actor].LastRequestTime)
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
			h := newCheckerTestHarness(t, nil)
			h.processor.DryRun = tt.dryRun

			h.addChain(BehavioralChain{
				Name:      "UnknownActionChain",
				MatchKey:  "ip",
				Action:    "throttle", // An unrecognized action
				StepsYAML: []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/test"}}},
			})

			entry := &LogEntry{
				IPInfo:    NewIPInfo("192.0.2.1"),
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
	resetGlobalState()

	chain := BehavioralChain{
		Name:  "CompletedChain",
		Steps: []StepDef{{Order: 1}}, // A simple one-step chain
	}

	// Create an activity state where the chain is already completed.
	activity := &ActorActivity{
		ChainProgress: map[string]StepState{
			"CompletedChain": {
				CurrentStep:   1, // CurrentStep (1) == len(chain.Steps) (1)
				LastMatchTime: time.Now(),
			},
		},
	}

	processor := newTestProcessor(nil, nil)

	entry := &LogEntry{} // Dummy entry, its contents don't matter for this test.

	// --- Act ---
	// Call the function under test. This should hit the 'if nextStepIndex >= len(chain.Steps)'
	// branch and immediately break.
	processChainForEntry(processor, &chain, entry, activity, time.Time{})

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
		step2YAML       StepDefYAML
		step2TimeOffset time.Duration
	}{
		{
			name:            "MaxDelay Exceeded",
			step2YAML:       StepDefYAML{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MaxDelay: "5s"},
			step2TimeOffset: 6 * time.Second,
		},
		{
			name:            "MinDelay Not Met",
			step2YAML:       StepDefYAML{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MinDelay: "500ms"},
			step2TimeOffset: 100 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			const targetIP = "192.0.2.1"
			h := newCheckerTestHarness(t, nil)

			h.addChain(BehavioralChain{
				Name:      "TimeResetChain",
				MatchKey:  "ip",
				Action:    "log",
				StepsYAML: []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/step/one"}}, tt.step2YAML},
			})

			// --- Act ---
			entry1 := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: time.Now(), Path: "/step/one"}
			h.processEntry(entry1)

			// Assert 1: State is at step 1.
			h.assertChainProgress("TimeResetChain", entry1, 1)

			// Process the second request with the specified time offset.
			entry2 := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: entry1.Timestamp.Add(tt.step2TimeOffset), Path: "/step/two"}
			h.processEntry(entry2)

			// --- Assert 2: The chain should have been reset.
			h.assertChainProgressCleared("TimeResetChain", entry2)
		})
	}
}

// TestCheckChains_WhitelistSkip tests that a whitelisted IP is skipped entirely.


// TestCheckChains_LogAction tests that a chain with Action="log" clears the state but does not call the blocker.
func TestCheckChains_LogAction(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.1"
	h := newCheckerTestHarness(t, nil)

	h.addChain(BehavioralChain{
		Name:          "LogActionTestChain",
		MatchKey:      "ip",
		Action:        "log", // ACTION: log
		BlockDuration: 0,
		StepsYAML: []StepDefYAML{
			{FieldMatches: map[string]interface{}{"Path": "/step/one"}, MaxDelay: "5s"},
			{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MaxDelay: "5s"},
		},
	})

	// --- Act ---
	entry1 := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: time.Now(), Path: "/step/one"}
	h.processEntry(entry1)

	// Assert 1: State is at step 1.
	h.assertChainProgress("LogActionTestChain", entry1, 1)

	// Process the second request (completion).
	entry2 := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: entry1.Timestamp.Add(2 * time.Second), Path: "/step/two"}
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
	h := newCheckerTestHarness(t, nil)
	h.addChain(BehavioralChain{
		Name:          "SingleStepChain",
		MatchKey:      "ip",
		Action:        "block",
		BlockDuration: 1 * time.Minute,
		StepsYAML:     []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/test"}}},
	})

	// Manually create a pre-existing, EXPIRED block state.
	actor := Actor{IPInfo: NewIPInfo(targetIP)}
	h.processor.ActivityStore[actor] = &ActorActivity{
		LastRequestTime: time.Time{},                    // Not relevant for this test
		BlockedUntil:    time.Now().Add(-1 * time.Hour), // Expired an hour ago
		IsBlocked:       true,
	}

	// --- Act ---
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/test",
	}
	h.processEntry(entry)

	// --- Assert ---
	h.assertBlocked(entry, true)
	if h.processor.ActivityStore[actor].BlockedUntil.Before(time.Now()) {
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
			h := newCheckerTestHarness(t, nil)
			h.addChain(BehavioralChain{
				Name:      "VersionMismatchChain",
				MatchKey:  tt.chainMatchKey,
				Action:    "log",
				StepsYAML: []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/test"}}},
			})

			// --- Act ---
			entry := &LogEntry{
				IPInfo:    NewIPInfo(tt.entryIP),
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

	h := newCheckerTestHarness(t, nil)
	h.addChain(BehavioralChain{
		Name:          "IP_UA_Blocker",
		MatchKey:      "ip_ua",
		Action:        "block",
		BlockDuration: 5 * time.Minute,
		StepsYAML:     []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/trigger"}}},
	})

	// --- Act ---
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: targetUA,
		Timestamp: time.Now(),
		Path:      "/trigger",
	}
	h.processEntry(entry)

	// --- Assert ---
	// Assert that the specific IP+UA key is blocked.
	h.assertBlocked(entry, true)
	// Assert that the general IP-only key is also blocked.
	ipOnlyEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), UserAgent: ""}
	h.assertBlocked(ipOnlyEntry, true)
}

// TestCheckChains_OnMatchStop verifies that when a chain with `on_match: "stop"`
// completes, no further chains are processed for that log entry.
func TestCheckChains_OnMatchStop(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.1"
	h := newCheckerTestHarness(t, nil)

	// Chain 1: This chain will match and has on_match: "stop".
	h.addChain(BehavioralChain{
		Name:      "StopChain",
		MatchKey:  "ip",
		Action:    "log",
		OnMatch:   "stop",
		StepsYAML: []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/trigger"}}},
	})

	// Chain 2: This chain also matches the same entry but should be skipped.
	h.addChain(BehavioralChain{
		Name:      "ShouldBeSkippedChain",
		MatchKey:  "ip",
		Action:    "log",
		StepsYAML: []StepDefYAML{{FieldMatches: map[string]interface{}{"Path": "/trigger"}}},
	})

	// --- Act ---
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/trigger",
	}
	h.processEntry(entry)

	// --- Assert ---
	// The "StopChain" should have completed and its progress cleared.
	h.assertChainProgressCleared("StopChain", entry)

	// The "ShouldBeSkippedChain" should have no progress state, as it was never processed.
	key := GetActor(&h.processor.Chains[1], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[key]
	if exists && len(activity.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress for 'ShouldBeSkippedChain' to be empty, but it has entries: %v", activity.ChainProgress)
	}
}

// TestCheckChains_TimeRules provides focused tests for the new time-based rules,
// especially the first-step-only `first_hit_since` logic.
func TestCheckChains_TimeRules(t *testing.T) {
	// 1. Setup
	chain := BehavioralChain{
		Name:     "TimeRuleTestChain",
		MatchKey: "ip",
		Action:   "log",
	}
	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step1", new([]string), "")
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step2", new([]string), "")
	chain.Steps = []StepDef{
		{Order: 1, MinTimeSinceLastHit: 2 * time.Second, Matchers: []fieldMatcher{matcher1}},
		{Order: 2, Matchers: []fieldMatcher{matcher2}},
	}

	chains := []BehavioralChain{chain}

	baseProcessor := func() *Processor {
		return newTestProcessor(&AppConfig{}, chains)
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
			name: "min_time_since_last_hit FAILURE - IP never seen before",
			// Zero time indicates the IP has never been seen, so the rule doesn't match.
			primingTimeOffset:   0,
			shouldChainProgress: false,
		},
	}

	// Use a fixed "now" for deterministic test runs.
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := baseProcessor()

			// Prime the activity store with the specified last request time.
			if tt.primingTimeOffset != 0 {
				key := Actor{IPInfo: NewIPInfo("192.0.2.1")}
				// Use GetOrCreateActorActivityUnsafe to ensure ChainProgress map is initialized.
				processor.ActivityMutex.Lock()
				activity := GetOrCreateActorActivityUnsafe(processor.ActivityStore, key)
				activity.LastRequestTime = now.Add(tt.primingTimeOffset)
				processor.ActivityMutex.Unlock()
			}

			entry := &LogEntry{
				IPInfo:    NewIPInfo("192.0.2.1"),
				Timestamp: now,      // The current request always happens at our fixed "now".
				Path:      "/step1", // This will match the first step
			}
			CheckChains(processor, entry)

			processor.ActivityMutex.RLock()
			activity := processor.ActivityStore[Actor{IPInfo: entry.IPInfo}]
			progressExists := activity.ChainProgress[chain.Name] != StepState{}
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
	resetGlobalState()

	// The config.yaml file now references a file matcher. We need to create it.
	tempDir := t.TempDir()
	uaFile := filepath.Join(tempDir, "bad_user_agents.txt")
	if err := os.WriteFile(uaFile, []byte("BadUA/1.0\nregex:NastyBot"), 0644); err != nil {
		t.Fatalf("Failed to create dummy user agent file: %v", err)
	}
	// The test config.yaml is hardcoded to look for this relative path.
	// We need to create it in the current working directory.
	// A better long-term solution would be to make the path in config.yaml absolute or configurable for tests.
	if err := os.WriteFile("bad_user_agents.txt", []byte("BadUA/1.0\nregex:NastyBot"), 0644); err != nil {
		t.Fatalf("Failed to create dummy user agent file in current directory: %v", err)
	}
	t.Cleanup(func() { os.Remove("bad_user_agents.txt") })
	// 1. Load configuration (chains, whitelist, etc.)
	loadedCfg, err := LoadConfigFromYAML("testdata/config.yaml")
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed: %v", err)
	}
	logging.SetLogLevel(loadedCfg.LogLevel)

	// Create a processor (but don't start any background processes like ChainWatcher).
	processor := &Processor{
		ActivityMutex:     &sync.RWMutex{},
		ActivityStore:     make(map[Actor]*ActorActivity),
		TopActorsPerChain: make(map[string]map[string]*ActorStats),
		ConfigMutex:       &sync.RWMutex{},
		Metrics:           NewMetrics(),
		Chains:            loadedCfg.Chains,
		Config: &AppConfig{
			OutOfOrderTolerance: 0, // Disable buffering for this specific test.
			MaxTimeSinceLastHit: loadedCfg.MaxTimeSinceLastHit,
			TimestampFormat:     loadedCfg.TimestampFormat,

		},
		DryRun:  true,                                                                            // Simulate dry-run mode
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {}, // Will be replaced
	}

	// Set the CheckChainsFunc on the processor instance to avoid nil pointers.
	processor.CheckChainsFunc = func(entry *LogEntry) { CheckChains(processor, entry) }
	processor.ProcessLogLine = func(line string) { processLogLineInternal(processor, line) }

	// 2. Read test_access.log and extract expected log outputs from comments
	testLogFilePath := "testdata/test_access.log"
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
		// Our custom LogFunc only captures the output. We do NOT call LogOutput here
		// to prevent verbose output during test runs unless explicitly requested via t.Logf.

		logLine := tag + ": " + fmt.Sprintf(format, args...)
		capturedLogs = append(capturedLogs, logLine)
	}

	// Read in each line of the test log, and run CheckChains on it, to simulate log tailing.
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
	for commentLineNumber, expectedLog := range expectedLogs {
		found := false
		formattedExpectedLog := strings.Replace(expectedLog, "Line %d: ", "", 1)

		for _, capturedLog := range capturedLogs {
			if strings.Contains(capturedLog, formattedExpectedLog) {
				found = true
				break // Found the expected log message
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
		{
			name:             "Out-of-order within tolerance (processed)",
			outOfOrderOffset: 3 * time.Second, // 3s older
			tolerance:        5 * time.Second, // 5s tolerance
			expectProcessed:  true,
		},
		// The test's `expectLastRequestTime` assertion was hardcoded to `now`, which is incorrect for the in-order case.
		{
			name:             "Out-of-order exactly at tolerance (processed)",
			outOfOrderOffset: 5 * time.Second, // 5s older
			tolerance:        5 * time.Second, // 5s tolerance
			expectProcessed:  true,
		},
		{
			name:             "Out-of-order outside tolerance (processed after delay)",
			outOfOrderOffset: 6 * time.Second, // 6s older
			tolerance:        5 * time.Second, // 5s tolerance
			expectProcessed:  true,            // It will be buffered and processed later, so the chain should still progress.
		},
		{
			name:             "In-order entry (processed)",
			outOfOrderOffset: -1 * time.Second, // 1s newer
			tolerance:        5 * time.Second,
			expectProcessed:  true,
		}, // The test's `expectLastRequestTime` assertion was hardcoded to `now`, which is incorrect for the in-order case.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobalState()
			targetIP := "192.0.2.1"
			chain := BehavioralChain{
				Name:     "SimpleChain",
				MatchKey: "ip",
				Action:   "log",
			}
			matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step1", new([]string), "")
			matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step2", new([]string), "")
			chain.Steps = []StepDef{
				{Order: 1, Matchers: []fieldMatcher{matcher1}},
				{Order: 2, Matchers: []fieldMatcher{matcher2}},
			}
			chains := []BehavioralChain{chain}

			processor := newTestProcessor(&AppConfig{OutOfOrderTolerance: tt.tolerance, MaxTimeSinceLastHit: 1 * time.Minute}, chains)

			// Manually create an activity to ensure the ChainProgress map is not nil.
			key := Actor{IPInfo: NewIPInfo(targetIP)}
			GetOrCreateActorActivityUnsafe(processor.ActivityStore, key)

			// 1. Process a "newer" entry first to set the LastRequestTime.
			newerEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: now, Path: "/other-path"}
			CheckChains(processor, newerEntry)

			// 2. Process the out-of-order entry.
			outOfOrderEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: now.Add(-tt.outOfOrderOffset), Path: "/step1"}
			CheckChains(processor, outOfOrderEntry)

			// Manually flush the buffer to process the entries for the test.
			processor.ActivityMutex.Lock()
			sort.Slice(processor.EntryBuffer, func(i, j int) bool {
				return processor.EntryBuffer[i].Timestamp.Before(processor.EntryBuffer[j].Timestamp)
			})
			for _, entry := range processor.EntryBuffer {
				checkChainsInternal(processor, entry)
			}
			processor.EntryBuffer = nil
			processor.ActivityMutex.Unlock()

			// 3. Assert the outcome.
			processor.ActivityMutex.RLock()
			defer processor.ActivityMutex.RUnlock()
			activity := processor.ActivityStore[Actor{IPInfo: NewIPInfo(targetIP)}]

			_, progressExists := activity.ChainProgress[chain.Name]

			if progressExists != tt.expectProcessed {
				t.Errorf("Expected chain progress existence to be %t, but got %t", tt.expectProcessed, progressExists)
			}

			// Determine the expected LastRequestTime based on the test case.
			expectedLRT := now           // Default: if out-of-order or skipped, it should remain 'now' from newerEntry.
			if tt.outOfOrderOffset < 0 { // This means the outOfOrderEntry's timestamp is newer than 'now'.
				expectedLRT = now.Add(-tt.outOfOrderOffset)
			}

			// Verify LastRequestTime monotonicity.
			if !activity.LastRequestTime.Equal(expectedLRT) {
				t.Errorf("LastRequestTime was %v, expected %v (should always be the latest seen)", activity.LastRequestTime, expectedLRT)
			}
		})
	}
}

// bufferWorkerTestHarness encapsulates the setup for testing the entryBufferWorker.
type bufferWorkerTestHarness struct {
	t             *testing.T
	processor     *Processor
	processed     []*LogEntry // Captures entries in the order they are processed.
	processMutex  sync.Mutex
	stopCh        chan struct{}
	doneCh        chan struct{}
	tickDoneCh    chan struct{} // Signals that a ticker cycle has completed.
	originalCheck func(p *Processor, entry *LogEntry)
}

// newBufferWorkerTestHarness creates a harness for testing the entryBufferWorker.
func newBufferWorkerTestHarness(t *testing.T, tolerance time.Duration) *bufferWorkerTestHarness {
	t.Helper()

	h := &bufferWorkerTestHarness{
		t:          t,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		tickDoneCh: make(chan struct{}, 1), // Buffered to prevent blocking
	}

	// Create a processor with a mock checkChainsInternal function to capture processed entries.
	p := newTestProcessor(&AppConfig{OutOfOrderTolerance: tolerance}, nil)
	h.processor = p

	// Replace the internal check function with our mock.
	h.originalCheck = checkChainsInternal
	checkChainsInternal = func(p *Processor, entry *LogEntry) {
		h.processMutex.Lock()
		defer h.processMutex.Unlock()
		h.processed = append(h.processed, entry)
	}

	// Override the LogFunc to detect when the worker has finished a tick.
	p.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Only signal completion when the specific "tick done" message is seen.
		if tag == "BUFFER_WORKER_TICK_DONE" {
			h.tickDoneCh <- struct{}{}
		}
	}

	t.Cleanup(func() {
		checkChainsInternal = h.originalCheck // Restore the original function after the test.
	})

	return h
}

// start runs the entryBufferWorker in a goroutine.
func (h *bufferWorkerTestHarness) start() {
	go func() {
		entryBufferWorker(h.processor, h.stopCh)
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
		h.t.Fatal("timed out waiting for entryBufferWorker to shut down")
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
	e1 := &LogEntry{Timestamp: baseTime.Add(-3 * tolerance), Path: "/path1"} // Oldest
	e2 := &LogEntry{Timestamp: baseTime.Add(-tolerance / 2), Path: "/path2"} // New
	e3 := &LogEntry{Timestamp: baseTime.Add(-2 * tolerance), Path: "/path3"}
	e4 := &LogEntry{Timestamp: baseTime, Path: "/path4"} // Newest

	// Add entries to the buffer out of order.
	h.processor.EntryBuffer = []*LogEntry{e2, e1, e4, e3}

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
	h.processMutex.Lock()
	if len(h.processed) != 2 {
		t.Fatalf("Expected 2 entries to be processed, but got %d", len(h.processed))
	}
	// Verify they were processed in chronological order (e1, then e3).
	if h.processed[0].Path != "/path1" || h.processed[1].Path != "/path3" {
		t.Errorf("Entries processed in wrong order. Got: [%s, %s], Want: [/path1, /path3]", h.processed[0].Path, h.processed[1].Path)
	}
	h.processMutex.Unlock()

	// Verify the remaining entries are still in the buffer.
	h.processor.ActivityMutex.Lock()
	if len(h.processor.EntryBuffer) != 2 {
		t.Fatalf("Expected 2 entries to remain in buffer, but got %d", len(h.processor.EntryBuffer))
	}
	h.processor.ActivityMutex.Unlock()

	// --- Act 2: Stop the worker to trigger shutdown processing ---
	h.stop()

	// --- Assert 2: Check that all remaining entries were flushed and processed ---
	h.processMutex.Lock()
	if len(h.processed) != 4 {
		t.Fatalf("Expected all 4 entries to be processed after shutdown, but got %d", len(h.processed))
	}
	// The final two entries (e2, e4) should have been processed, also in order.
	if h.processed[2].Path != "/path2" || h.processed[3].Path != "/path4" {
		t.Errorf("Shutdown entries processed in wrong order. Got: [%s, %s], Want: [/path2, /path4]", h.processed[2].Path, h.processed[3].Path)
	}
	h.processMutex.Unlock()
}
