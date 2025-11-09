package main

import (
	"bot-detector/internal/logging"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Test Case for CheckChains --

// TestCheckChains_SuccessfulBlock tests a multi-step chain that successfully triggers a block.
func TestCheckChains_SuccessfulBlock(t *testing.T) {
	// --- Setup ---
	const targetIP = "192.0.2.1"
	const blockDuration = 10 * time.Minute

	h := newCheckerTestHarness(t, nil)

	// Define and add a two-step chain to the harness
	h.addChain(BehavioralChain{
		Name:          "TwoStepPathBlocker",
		MatchKey:      "ip_ua",
		Action:        "block",
		BlockDuration: blockDuration,
		StepsYAML: []StepDefYAML{
			{FieldMatches: map[string]interface{}{"Path": "/step/one"}, MaxDelay: "5s"},
			{FieldMatches: map[string]interface{}{"Path": "/step/two"}, MaxDelay: "5s"},
		},
	})

	// --- STEP 1: Process the first request --
	entry1 := &LogEntry{IPInfo: NewIPInfo(targetIP), UserAgent: "TestAgent", Timestamp: time.Now(), Path: "/step/one"}
	h.processEntry(entry1)

	// Assert 1: No block should be called, and the state should be at step 1
	if h.blockCalled {
		t.Fatal("Blocker was called after step 1, but it should only be called after step 2.")
	}

	h.assertChainProgress("TwoStepPathBlocker", entry1, 1)

	// --- STEP 2: Process the second request (the attack completion) --

	entry2 := &LogEntry{IPInfo: NewIPInfo(targetIP), UserAgent: "TestAgent", Timestamp: entry1.Timestamp.Add(2 * time.Second), Path: "/step/two"}
	h.processEntry(entry2)

	// Assert 2: Block was called, and activity state is updated
	if !h.blockCalled {
		t.Fatal("Expected Blocker to be called after completing the chain, but it was not.")
	}

	if h.blockCallArgs.ipInfo.Address != targetIP {
		t.Errorf("Blocker called with incorrect IP. Got %s, want %s", h.blockCallArgs.ipInfo.Address, targetIP)
	}
	if h.blockCallArgs.duration != blockDuration {
		t.Errorf("Blocker called with incorrect duration. Got %v, want %v", h.blockCallArgs.duration, blockDuration)
	}

	// Check final ActivityStore state: IsBlocked should be true and ChainProgress should be cleared
	h.assertBlocked(entry2, true)
	h.assertChainProgressCleared("TwoStepPathBlocker", entry2)
}

// TestPreCheckActivity_StillBlocked_OldEntry verifies that when an IP is already blocked,
// and an out-of-order (older) log entry arrives, the LastRequestTime is NOT updated.
func TestPreCheckActivity_StillBlocked_OldEntry(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	const targetIP = "192.0.2.50"
	trackingKey := TrackingKey{IPInfo: NewIPInfo(targetIP)}
	now := time.Now()

	processor := newTestProcessor(nil, nil)

	// 2. Manually create a pre-existing, non-expired block state.
	// The last request was seen at time 'now'.
	processor.ActivityStore[trackingKey] = &BotActivity{
		LastRequestTime: now,
		BlockedUntil:    now.Add(1 * time.Hour),
		IsBlocked:       true,
	}

	// 3. Create a log entry with a timestamp OLDER than the last seen request.
	oldEntry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: now.Add(-10 * time.Second), // 10 seconds in the past
	}

	// --- Act ---
	// The preCheckActivity function is not exported, so we call CheckChains,
	// which calls it internally. We lock the mutex to inspect the result.
	processor.ActivityMutex.Lock()
	_, skip := preCheckActivity(processor, oldEntry, trackingKey)
	processor.ActivityMutex.Unlock()

	// --- Assert ---
	if !skip {
		t.Error("Expected skip to be true for an already-blocked IP, but it was false.")
	}

	// Assert that the LastRequestTime was NOT updated because the incoming entry was older.
	processor.ActivityMutex.RLock()
	finalActivity := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

	if !finalActivity.LastRequestTime.Equal(now) {
		t.Errorf("Expected LastRequestTime to remain unchanged, but it was updated from %v to %v",
			now, finalActivity.LastRequestTime)
	}
}

// TestPreCheckActivity_StillBlocked_NewEntry verifies that when an IP is already blocked,
// and a newer log entry arrives, the LastRequestTime IS updated.
func TestPreCheckActivity_StillBlocked_NewEntry(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	const targetIP = "192.0.2.51"
	trackingKey := TrackingKey{IPInfo: NewIPInfo(targetIP)}
	now := time.Now()

	processor := newTestProcessor(nil, nil)

	// 2. Manually create a pre-existing, non-expired block state.
	// The last request was seen at time 'now'.
	processor.ActivityStore[trackingKey] = &BotActivity{
		LastRequestTime: now,
		BlockedUntil:    now.Add(1 * time.Hour),
		IsBlocked:       true,
	}

	// 3. Create a log entry with a timestamp NEWER than the last seen request.
	newEntryTimestamp := now.Add(10 * time.Second)
	newEntry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: newEntryTimestamp,
	}

	// --- Act ---
	processor.ActivityMutex.Lock()
	_, skip := preCheckActivity(processor, newEntry, trackingKey)
	processor.ActivityMutex.Unlock()

	// --- Assert ---
	if !skip {
		t.Error("Expected skip to be true for an already-blocked IP, but it was false.")
	}

	if !processor.ActivityStore[trackingKey].LastRequestTime.Equal(newEntryTimestamp) {
		t.Errorf("Expected LastRequestTime to be updated to %v, but it was %v",
			newEntryTimestamp, processor.ActivityStore[trackingKey].LastRequestTime)
	}
}

// TestCheckChains_DryRun tests that a block is NOT executed when DryRun is true.
func TestCheckChains_DryRun(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState() // Clean state

	const targetIP = "192.0.2.1"
	const blockDuration = 10 * time.Minute

	// Log entry template
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: "TestAgent",
		Timestamp: time.Now().Add(-1 * time.Second),
		Path:      "/step/one",
	}

	// Define a simple two-step chain
	chain := BehavioralChain{
		Name:          "DryRunTestChain",
		MatchKey:      "ip", // Use ip-only for simplicity
		Action:        "block",
		BlockDuration: blockDuration,
	}

	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step/one", new([]string))
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step/two", new([]string))

	chain.Steps = []StepDef{
		{Order: 1, Matchers: []fieldMatcher{matcher1}, MaxDelayDuration: 5 * time.Second},
		{Order: 2, Matchers: []fieldMatcher{matcher2}, MaxDelayDuration: 5 * time.Second},
	}

	chains := []BehavioralChain{chain}

	// Setup a mock blocker to intercept the block call
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor with DryRun=true
	processor := newTestProcessor(&AppConfig{}, chains)
	processor.DryRun = true
	processor.Blocker = mockBlocker

	// Get the correct tracking key for the activity store (ip-only)
	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1 & 2: Process both steps (which should trigger a "block" in dry run) --
	CheckChains(processor, entry) // Step 1
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"
	CheckChains(processor, entry) // Step 2 (completion)

	// Assertions
	if blockCalled {
		t.Fatal("Blocker was called, but should be skipped in DryRun mode.")
	}

	processor.ActivityMutex.RLock()
	activityFinal, exists := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

	if !exists {
		t.Fatal("Expected DryRunActivityStore to have activity, but it did not.")
	}
	// The in-memory block should still happen in the DryRun store
	if !activityFinal.IsBlocked {
		t.Error("Expected DryRun activity state to be IsBlocked=true, but was false.")
	}
}

// TestCheckChains_DryRun_UnknownAction tests that an unrecognized action is handled gracefully in dry-run mode.
func TestCheckChains_DryRun_UnknownAction(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	// Define a chain with an action that is not 'block' or 'log'.
	chain := BehavioralChain{
		Name:     "UnknownActionChain",
		MatchKey: "ip",
		Action:   "throttle", // An unrecognized action
	}
	matcher, _ := compileStringMatcher(chain.Name, 0, "Path", "/test", new([]string))
	chain.Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher}}}

	chains := []BehavioralChain{chain}

	// Capture log output
	var capturedLog string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		if tag == "DRY_RUN" {
			capturedLog = fmt.Sprintf(format, args...)
		}
		logMutex.Unlock()
	}

	processor := newTestProcessor(&AppConfig{}, chains)
	processor.DryRun = true
	processor.LogFunc = logCaptureFunc

	entry := &LogEntry{
		IPInfo:    NewIPInfo("192.0.2.1"),
		Timestamp: time.Now(),
		Path:      "/test",
	}

	// --- Act ---
	CheckChains(processor, entry)

	// --- Assert ---
	expectedLogSubstring := "UNKNOWN_ACTION!"
	if !strings.Contains(capturedLog, expectedLogSubstring) {
		t.Errorf("Expected log to contain '%s' for unknown action, but got: '%s'", expectedLogSubstring, capturedLog)
	}

	// Also assert that no block state was created.
	processor.ActivityMutex.RLock()
	activity := processor.ActivityStore[GetTrackingKey(&chain, entry)]
	processor.ActivityMutex.RUnlock()
	if activity != nil && activity.IsBlocked {
		t.Error("IsBlocked should be false for an unknown action, but it was true.")
	}
}

// TestCheckChains_LiveMode_UnknownAction tests that an unrecognized action is handled gracefully in live mode.
func TestCheckChains_LiveMode_UnknownAction(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	// Define a chain with an action that is not 'block' or 'log'.
	chain := BehavioralChain{
		Name:     "LiveUnknownActionChain",
		MatchKey: "ip",
		Action:   "throttle", // An unrecognized action
	}
	matcher, _ := compileStringMatcher(chain.Name, 0, "Path", "/test", new([]string))
	chain.Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher}}}

	chains := []BehavioralChain{chain}

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	processor := newTestProcessor(&AppConfig{}, chains)
	processor.DryRun = false
	processor.Blocker = mockBlocker

	entry := &LogEntry{
		IPInfo:    NewIPInfo("192.0.2.1"),
		Timestamp: time.Now(),
		Path:      "/test",
	}

	// --- Act ---
	CheckChains(processor, entry)

	// --- Assert ---
	if blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for an unknown action.")
	}

	// Also assert that the chain progress was reset and no block state was created.
	processor.ActivityMutex.RLock()
	activity := processor.ActivityStore[GetTrackingKey(&chain, entry)]
	processor.ActivityMutex.RUnlock()

	if activity.IsBlocked {
		t.Error("IsBlocked should be false for an unknown action, but it was true.")
	}
	if len(activity.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be cleared, but it has %d entries.", len(activity.ChainProgress))
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
	activity := &BotActivity{
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

// TestCheckChains_MaxDelayExceeded tests that a chain resets if the time between steps is too long.
func TestCheckChains_MaxDelayExceeded(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState() // Clean state

	const targetIP = "192.0.2.1"

	// Log entry template
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: "TestAgent",
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Define a two-step chain with a 5s max delay
	chain := BehavioralChain{
		Name:     "DelayTestChain",
		MatchKey: "ip",
		Action:   "log", // Don't block, just log for simplicity
	}

	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step/one", new([]string))
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step/two", new([]string))

	chain.Steps = []StepDef{
		{Order: 1, Matchers: []fieldMatcher{matcher1}, MaxDelayDuration: 5 * time.Second},
		{Order: 2, Matchers: []fieldMatcher{matcher2}, MaxDelayDuration: 5 * time.Second},
	}

	chains := []BehavioralChain{chain}

	processor := newTestProcessor(&AppConfig{}, chains)

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	CheckChains(processor, entry)

	// Assert 1: State is at step 1
	processor.ActivityMutex.RLock()
	activity := processor.ActivityStore[trackingKey]
	if activity.ChainProgress[chain.Name].CurrentStep != 1 {
		t.Fatalf("Expected state to be at step 1, got %d", activity.ChainProgress[chain.Name].CurrentStep)
	}
	processor.ActivityMutex.RUnlock() // <-- Release the read lock

	// --- STEP 2: Process the second request, after MaxDelay (e.g., 6 seconds later) ---

	entry.Timestamp = entry.Timestamp.Add(6 * time.Second) // > 5 seconds delay
	entry.Path = "/step/two"

	CheckChains(processor, entry)

	// Assert 2: Chain should have been reset, and the second step should be treated as the *first* step
	// of a new sequence, but it doesn't match step 1, so the chain progress should be cleared.
	processor.ActivityMutex.RLock()
	activityFinal := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be reset/cleared (length 0) after MaxDelay, but has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
	}
}

// TestCheckChains_MinDelayNotMet tests that a chain resets if the time between steps is too short.
func TestCheckChains_MinDelayNotMet(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState() // Clean state

	const targetIP = "192.0.2.1"

	// Log entry template
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: "TestAgent",
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Define a two-step chain with a 500ms min delay
	chain := BehavioralChain{
		Name:     "MinDelayTestChain",
		MatchKey: "ip",
		Action:   "log",
	}

	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step/one", new([]string))
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step/two", new([]string))

	chain.Steps = []StepDef{
		{Order: 1, Matchers: []fieldMatcher{matcher1}, MaxDelayDuration: 5 * time.Second},
		{Order: 2, Matchers: []fieldMatcher{matcher2}, MinDelayDuration: 500 * time.Millisecond},
	}

	chains := []BehavioralChain{chain}

	processor := newTestProcessor(&AppConfig{}, chains)

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	CheckChains(processor, entry)

	// Assert 1: State is at step 1
	processor.ActivityMutex.RLock()
	activity := processor.ActivityStore[trackingKey]
	if activity.ChainProgress[chain.Name].CurrentStep != 1 {
		t.Fatalf("Expected state to be at step 1, got %d", activity.ChainProgress[chain.Name].CurrentStep)
	}
	processor.ActivityMutex.RUnlock() // <-- Release the read lock

	// --- STEP 2: Process the second request, before MinDelay (e.g., 100ms later) ---

	entry.Timestamp = entry.Timestamp.Add(100 * time.Millisecond) // < 500ms delay
	entry.Path = "/step/two"

	CheckChains(processor, entry)

	// Assert 2: Chain should be reset because min delay was not met.
	processor.ActivityMutex.RLock()
	activityFinal := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be reset/cleared (length 0) after MinDelay failure, but has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
	}
}

// TestCheckChains_WhitelistSkip tests that a whitelisted IP is skipped entirely.
func TestCheckChains_WhitelistSkip(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState() // Clean state

	const targetIP = "192.0.2.1"
	const whitelistedIP = "192.168.0.10"
	const blockDuration = 10 * time.Minute

	// Define the chain (it doesn't need to be complex)
	chain := BehavioralChain{
		Name:          "WhitelistTestChain",
		MatchKey:      "ip",
		Action:        "block",
		BlockDuration: blockDuration,
	}
	logChain := BehavioralChain{
		Name:     "WhitelistLogChain",
		MatchKey: "ip",
		Action:   "log", // This chain only logs
	}

	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step/one", new([]string))
	chain.Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher1}}}

	matcher2, _ := compileStringMatcher(logChain.Name, 0, "Path", "/log/step", new([]string))
	logChain.Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher2}}}

	chains := []BehavioralChain{chain, logChain} // Include both chains

	// Log entry template for whitelisted IP
	whitelistedEntry := &LogEntry{
		IPInfo:    NewIPInfo(whitelistedIP),
		UserAgent: "TestAgent",
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Setup whitelisting using a mock function for IsIPWhitelisted
	mockIsWhitelisted := func(ipInfo IPInfo) bool {
		return ipInfo.Address == whitelistedIP
	}

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Capture log output
	var capturedLogs []string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
		logMutex.Unlock()
	}

	// Create the processor
	processor := newTestProcessor(&AppConfig{}, chains)
	processor.Blocker = mockBlocker
	processor.IsWhitelistedFunc = mockIsWhitelisted
	processor.LogFunc = logCaptureFunc

	whitelistedKey := GetTrackingKey(&chain, whitelistedEntry)

	// --- ACT: Process the whitelisted request ---
	CheckChains(processor, whitelistedEntry) // Process the 'block' action chain

	// --- Assertions for Whitelisted IP (should be skipped) ---
	var activity *BotActivity
	var exists bool
	processor.ActivityMutex.RLock() // Lock before accessing the map
	activity, exists = processor.ActivityStore[whitelistedKey]
	processor.ActivityMutex.RUnlock()

	if blockCalled {
		t.Fatal("Blocker was called, but should be skipped for a whitelisted IP.")
	}

	if exists {
		t.Errorf("Activity state exists for whitelisted IP %s, but it should have been skipped entirely.", whitelistedIP)
	}

	// Bonus check: ensure a non-whitelisted IP works normally
	nonWhitelistedEntry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now().Add(1 * time.Second),
		Path:      "/step/one",
	}

	// --- ACT: Process the non-whitelisted request ---
	processor.CheckChainsFunc = func(entry *LogEntry) { CheckChains(processor, entry) } // Set the real method for this part
	CheckChains(processor, nonWhitelistedEntry)
	nonWhitelistedKey := GetTrackingKey(&chain, nonWhitelistedEntry)

	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()
	activity, exists = processor.ActivityStore[nonWhitelistedKey]
	if !exists {
		t.Error("Activity state for non-whitelisted IP should exist, but did not.")
	} else {
		// A single-step chain that matches is completed and clears ChainProgress.
		if !activity.IsBlocked {
			t.Error("Expected non-whitelisted IP to be marked as IsBlocked=true after single-step chain completion, but was false.")
		}
		if len(activity.ChainProgress) != 0 {
			t.Errorf("ChainProgress should be cleared on chain completion (length 0) but has %d entries: %v", len(activity.ChainProgress), activity.ChainProgress)
		}
	}
}

// TestCheckChains_LogAction tests that a chain with Action="log" clears the state but does not call the blocker.
func TestCheckChains_LogAction(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState()

	const targetIP = "192.0.2.1"

	// Log entry template
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Define a two-step chain with Action="log"
	chain := BehavioralChain{
		Name:          "LogActionTestChain",
		MatchKey:      "ip",
		Action:        "log", // ACTION: log
		BlockDuration: 0,
	}

	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step/one", new([]string))
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step/two", new([]string))

	chain.Steps = []StepDef{
		{Order: 1, Matchers: []fieldMatcher{matcher1}, MaxDelayDuration: 5 * time.Second},
		{Order: 2, Matchers: []fieldMatcher{matcher2}, MaxDelayDuration: 5 * time.Second},
	}

	chains := []BehavioralChain{chain}

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := newTestProcessor(&AppConfig{}, chains)
	processor.Blocker = mockBlocker

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	CheckChains(processor, entry)

	// Assert 1: State is at step 1
	processor.ActivityMutex.RLock()
	activity := processor.ActivityStore[trackingKey]
	if len(activity.ChainProgress) == 0 {
		t.Fatal("Expected state to exist after step 1, but it was empty.")
	}
	processor.ActivityMutex.RUnlock()

	// --- STEP 2: Process the second request (completion) ---
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"

	CheckChains(processor, entry)

	// Assert 2: Block was NOT called, but ChainProgress should be cleared
	if blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for Action='log'.")
	}

	processor.ActivityMutex.RLock()
	activityFinal := processor.ActivityStore[trackingKey]

	// For a 'log' action, IsBlocked should be false and ChainProgress should be cleared
	if activityFinal.IsBlocked {
		t.Error("Expected final activity state to be IsBlocked=false for 'log' action, but was true.")
	}
	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be cleared for 'log' action, but has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
	}
}

// TestCheckChains_LogAction_Whitelisted verifies that when a whitelisted IP completes
// a chain with action: "log", a specific log message is generated and no block occurs.
func TestCheckChains_LogAction_Whitelisted(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	const whitelistedIP = "192.0.2.10"
	chain := BehavioralChain{
		Name:   "LogOnlyForWhitelist",
		Action: "log",
		Steps:  []StepDef{{Order: 1}},
	}
	matcher, _ := compileStringMatcher(chain.Name, 0, "Path", "/trigger", new([]string))
	chain.Steps[0].Matchers = []fieldMatcher{matcher}

	// Capture log output
	var capturedLog string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		if tag == "ALERT" {
			capturedLog = fmt.Sprintf(format, args...)
		}
	}

	processor := newTestProcessor(&AppConfig{}, []BehavioralChain{chain})
	processor.IsWhitelistedFunc = func(ipInfo IPInfo) bool {
		return ipInfo.Address == whitelistedIP
	}
	processor.LogFunc = logCaptureFunc

	entry := &LogEntry{
		IPInfo:    NewIPInfo(whitelistedIP),
		Timestamp: time.Now(),
		Path:      "/trigger",
	}

	// Set the CheckChainsFunc to the real method on the processor instance.
	processor.CheckChainsFunc = func(entry *LogEntry) { CheckChains(processor, entry) }

	// --- Act ---
	CheckChains(processor, entry)

	// --- Assert ---
	// With the corrected logic, CheckChains should exit immediately for a whitelisted IP.
	// Therefore, no "ALERT" log should be generated.
	if capturedLog != "" {
		t.Errorf("Expected no log message for a whitelisted IP, but got: '%s'", capturedLog)
	}
}

// TestCheckChains_UnrecognizedAction tests that a chain with an unrecognized action only logs and clears state.
func TestCheckChains_UnrecognizedAction(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState()

	const targetIP = "192.0.2.1"

	const blockDuration = 1 * time.Minute
	// Log entry template
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Define a two-step chain with an unrecognized action
	chain := BehavioralChain{
		Name:          "UnknownActionTestChain",
		MatchKey:      "ip",
		Action:        "unknown", // ACTION: unknown
		BlockDuration: blockDuration,
	}

	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step/one", new([]string))
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step/two", new([]string))

	chain.Steps = []StepDef{
		{Order: 1, Matchers: []fieldMatcher{matcher1}, MaxDelayDuration: 5 * time.Second},
		{Order: 2, Matchers: []fieldMatcher{matcher2}, MaxDelayDuration: 5 * time.Second},
	}

	chains := []BehavioralChain{chain}

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := newTestProcessor(&AppConfig{}, chains)
	processor.Blocker = mockBlocker

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	CheckChains(processor, entry)

	// --- STEP 2: Process the second request (completion) ---
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"

	CheckChains(processor, entry)

	// Assert 2: Block was NOT called, and ChainProgress should be cleared
	if blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for Action='unknown'.")
	}

	processor.ActivityMutex.RLock()
	activityFinal := processor.ActivityStore[trackingKey]

	// For an 'unknown' action, IsBlocked should be false and ChainProgress should be cleared
	if activityFinal.IsBlocked {
		t.Error("Expected final activity state to be IsBlocked=false for 'unknown' action, but was true.")
	}
	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be cleared for 'unknown' action, but has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
	}
}

// TestCheckChains_BlockExpiration verifies that if an activity is marked as blocked but the
// BlockedUntil time has passed, the block is cleared and the chain can be processed again.
func TestCheckChains_BlockExpiration(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	const targetIP = "192.0.2.1"
	chain := BehavioralChain{
		Name:          "SingleStepChain",
		MatchKey:      "ip",
		Action:        "block",
		BlockDuration: 1 * time.Minute,
	}
	matcher, _ := compileStringMatcher(chain.Name, 0, "Path", "/test", new([]string))
	chain.Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher}}}

	chains := []BehavioralChain{chain}

	processor := newTestProcessor(&AppConfig{}, chains)

	// 2. Manually create a pre-existing, EXPIRED block state.
	trackingKey := GetTrackingKey(&chain, &LogEntry{IPInfo: NewIPInfo(targetIP)})
	processor.ActivityStore[trackingKey] = &BotActivity{
		LastRequestTime: time.Time{},                    // Not relevant for this test
		BlockedUntil:    time.Now().Add(-1 * time.Hour), // Expired an hour ago
		IsBlocked:       true,
	}

	// 3. Create the log entry that will be processed.
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/test",
	}

	// --- Act ---
	CheckChains(processor, entry)

	// --- Assert ---
	processor.ActivityMutex.RLock()
	finalActivity, exists := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

	if !exists {
		t.Fatal("Expected activity state to exist, but it was deleted.")
	}

	// The chain should have run, completed, and re-blocked the IP with a new expiration.
	if !finalActivity.IsBlocked {
		t.Error("Expected IsBlocked to be true after re-processing, but it was false.")
	}

	if finalActivity.BlockedUntil.Before(time.Now()) {
		t.Error("Expected BlockedUntil time to be in the future, but it was not.")
	}
}

// TestCheckChains_IPVersionMismatch verifies that chains are correctly skipped
// if the log entry's IP version does not match the chain's `match_key`.
func TestCheckChains_IPVersionMismatch(t *testing.T) {
	resetGlobalState()

	// 1. Define one chain for IPv4 and one for IPv6.
	chains := []BehavioralChain{
		{
			Name:     "IPv4-Only-Chain",
			MatchKey: "ipv4",
			Action:   "log",
		},
		{
			Name:     "IPv6-Only-Chain",
			MatchKey: "ipv6",
			Action:   "log",
		},
	}
	matcher, _ := compileStringMatcher("any", 0, "Path", "/test", new([]string))
	chains[0].Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher}}}
	chains[1].Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher}}}
	processor := newTestProcessor(&AppConfig{}, chains)

	// 2. Process an IPv4 log entry.
	entry := &LogEntry{
		IPInfo:    NewIPInfo("192.0.2.1"),
		Timestamp: time.Now(),
		Path:      "/test",
	}
	CheckChains(processor, entry)

	// 3. Assert the state.
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	activity := processor.ActivityStore[TrackingKey{IPInfo: entry.IPInfo}]

	// The IPv6 chain should have been skipped, so no progress should be recorded for it.
	if _, exists := activity.ChainProgress["IPv6-Only-Chain"]; exists {
		t.Error("Expected IPv6-Only-Chain to be skipped for an IPv4 log entry, but its state was created.")
	}
}

// TestCheckChains_IPAndUABlockOptimization verifies that when a chain with `match_key: "ip_ua"`
// completes a block action, it blocks BOTH the specific ip_ua key and the general ip-only key.
func TestCheckChains_IPAndUABlockOptimization(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	const targetIP = "192.0.2.100"
	const targetUA = "BadBot/1.0"

	chain := BehavioralChain{
		Name:          "IP_UA_Blocker",
		MatchKey:      "ip_ua",
		Action:        "block",
		BlockDuration: 5 * time.Minute,
	}
	matcher, _ := compileStringMatcher(chain.Name, 0, "Path", "/trigger", new([]string))
	chain.Steps = []StepDef{{Order: 1, Matchers: []fieldMatcher{matcher}}}
	chains := []BehavioralChain{chain}
	processor := newTestProcessor(&AppConfig{}, chains)

	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: targetUA,
		Timestamp: time.Now(),
		Path:      "/trigger",
	}

	// --- Act ---
	CheckChains(processor, entry)

	// --- Assert ---
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	ipUaKey := TrackingKey{IPInfo: NewIPInfo(targetIP), UA: targetUA}
	ipOnlyKey := TrackingKey{IPInfo: NewIPInfo(targetIP), UA: ""}

	if activity, exists := processor.ActivityStore[ipUaKey]; !exists || !activity.IsBlocked {
		t.Error("Expected the specific IP+UA key to be blocked, but it was not.")
	}
	if activity, exists := processor.ActivityStore[ipOnlyKey]; !exists || !activity.IsBlocked {
		t.Error("Expected the general IP-only key to be blocked for optimization, but it was not.")
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
	matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step1", new([]string))
	matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step2", new([]string))
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
				key := TrackingKey{IPInfo: NewIPInfo("192.0.2.1")}
				// Use GetOrCreateActivityUnsafe to ensure ChainProgress map is initialized.
				processor.ActivityMutex.Lock()
				activity := GetOrCreateActivityUnsafe(processor.ActivityStore, key)
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
			activity := processor.ActivityStore[TrackingKey{IPInfo: entry.IPInfo}]
			progressExists := activity.ChainProgress[chain.Name] != StepState{}
			if progressExists != tt.shouldChainProgress {
				t.Errorf("Chain progress existence was %t, but expected %t", progressExists, tt.shouldChainProgress)
			}
		})
	}
}

// TestDryRunMode simulates the entire dry-run process using chains.yaml and test_access.log,
// and verifies the log output against the expected log messages extracted from comments
// in test_access.log.
func TestDryRunMode(t *testing.T) {
	// --- Setup ---
	resetGlobalState()

	// Set required flags for this test
	originalYAMLPath := YAMLFilePath
	YAMLFilePath = "testdata/chains.yaml" // Assume it exists for this test
	t.Cleanup(func() { YAMLFilePath = originalYAMLPath })

	// The chains.yaml file now references a file matcher. We need to create it.
	tempDir := t.TempDir()
	uaFile := filepath.Join(tempDir, "bad_user_agents.txt")
	if err := os.WriteFile(uaFile, []byte("BadUA/1.0\nregex:NastyBot"), 0644); err != nil {
		t.Fatalf("Failed to create dummy user agent file: %v", err)
	}
	// The test chains.yaml is hardcoded to look for this relative path.
	// We need to create it in the current working directory.
	// A better long-term solution would be to make the path in chains.yaml absolute or configurable for tests.
	os.WriteFile("bad_user_agents.txt", []byte("BadUA/1.0\nregex:NastyBot"), 0644)
	t.Cleanup(func() { os.Remove("bad_user_agents.txt") })
	// 1. Load configuration (chains, whitelist, etc.)
	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed: %v", err)
	}
	logging.SetLogLevel(loadedCfg.LogLevel)

	// Create a processor (but don't start any background processes like ChainWatcher).
	processor := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		Config: &AppConfig{
			OutOfOrderTolerance: loadedCfg.OutOfOrderTolerance,
			MaxTimeSinceLastHit: loadedCfg.MaxTimeSinceLastHit,
			TimestampFormat:     loadedCfg.TimestampFormat,
			WhitelistNets:       loadedCfg.WhitelistNets,
		},
		DryRun:  true,                                                                            // Simulate dry-run mode
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {}, // Will be replaced
	}
	// Set the IsWhitelistedFunc on the *actual* processor instance to avoid nil pointers.
	processor.IsWhitelistedFunc = func(ipInfo IPInfo) bool { return IsIPWhitelisted(processor, ipInfo) }
	// Set the CheckChainsFunc on the processor instance to avoid nil pointers.
	processor.CheckChainsFunc = func(entry *LogEntry) { CheckChains(processor, entry) } // This line was correct, but included for context
	processor.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(processor, line, lineNumber) }

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

		processor.ProcessLogLine(line, actualLogLineNumber) // Use the actual processing function
	}
	if err := logEntryScanner.Err(); err != nil {
		t.Fatalf("Failed to scan test_access.log for entries: %v", err)
	}

	// --- Assert ---
	// 4. Verify that the captured log output matches the expected log entries
	for commentLineNumber, expectedLog := range expectedLogs {
		found := false
		formattedExpectedLog := expectedLog

		// If the expected log contains a line number placeholder, format it dynamically.
		if strings.Contains(expectedLog, "Line %d:") {
			// The malformed log entry is on the line immediately after the '======' separator, which is 2 lines after the 'EXPECTED LOG' comment.
			formattedExpectedLog = fmt.Sprintf(expectedLog, commentLineNumber+2)
		}

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
			name:                         "Out-of-order outside tolerance (skipped)",
			outOfOrderOffset:             6 * time.Second, // 6s older
			tolerance:                    5 * time.Second, // 5s tolerance
			expectProcessed:              false,
			expectedFinalLastRequestTime: now, // LastRequestTime should remain 'now' because outOfOrderEntry was skipped.
		},
		{
			name:             "In-order entry (processed)",
			outOfOrderOffset: -1 * time.Second, // 1s newer
			tolerance:        5 * time.Second,
			expectProcessed:  true,
		},
		// The test's `expectLastRequestTime` assertion was hardcoded to `now`, which is incorrect for the in-order case.
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
			matcher1, _ := compileStringMatcher(chain.Name, 0, "Path", "/step1", new([]string))
			matcher2, _ := compileStringMatcher(chain.Name, 1, "Path", "/step2", new([]string))
			chain.Steps = []StepDef{
				{Order: 1, Matchers: []fieldMatcher{matcher1}},
				{Order: 2, Matchers: []fieldMatcher{matcher2}},
			}
			chains := []BehavioralChain{chain}

			processor := newTestProcessor(&AppConfig{OutOfOrderTolerance: tt.tolerance, MaxTimeSinceLastHit: 1 * time.Minute}, chains)

			// 1. Process a "newer" entry first to set the LastRequestTime.
			newerEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: now, Path: "/other-path"}
			CheckChains(processor, newerEntry)

			// 2. Process the out-of-order entry.
			outOfOrderEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: now.Add(-tt.outOfOrderOffset), Path: "/step1"}
			CheckChains(processor, outOfOrderEntry)

			// 3. Assert the outcome.
			processor.ActivityMutex.RLock()
			defer processor.ActivityMutex.RUnlock()
			activity := processor.ActivityStore[TrackingKey{IPInfo: NewIPInfo(targetIP)}]

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
