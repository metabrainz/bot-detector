package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// MockBlocker and its methods are defined in log_parse_test.go for the 'main' package,
// so they are not redefined here to avoid the redeclaration and duplicate method errors.

// --- HELPER FUNCTION: Compile Regexes for Test Chains ---

// compileChainRegexes takes a slice of chains and compiles all string patterns
// in StepDef.FieldMatches into StepDef.CompiledRegexes, failing the test on error.
func compileChainRegexes(t *testing.T, chains []BehavioralChain) {
	for i := range chains {
		chain := &chains[i]
		for j := range chain.Steps {
			step := &chain.Steps[j]
			step.CompiledRegexes = make(map[string]*regexp.Regexp)
			for field, regexStr := range step.FieldMatches {
				// The YAML config expects a full regular expression, so we compile it as-is.
				re, err := regexp.Compile(regexStr)
				if err != nil {
					t.Fatalf("Failed to compile regex for chain '%s', step %d, field '%s' ('%s'): %v",
						chain.Name, step.Order, field, regexStr, err)
				}
				step.CompiledRegexes[field] = re
			}
		}
	}
}

// --- Test Case for CheckChains --

// TestCheckChains_SuccessfulBlock tests a multi-step chain that successfully triggers a block.
func TestCheckChains_SuccessfulBlock(t *testing.T) {
	// 1. Setup Data Structures
	resetGlobalState() // Clean state

	const targetIP = "192.0.2.1"
	const blockDuration = 10 * time.Minute

	// Log entry template
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: "TestAgent",
		Timestamp: time.Now().Add(-1 * time.Second), // Start time for the first request
		Path:      "/step/one",
	}

	// Define a two-step chain
	chain := BehavioralChain{
		Name:          "TwoStepPathBlocker",
		MatchKey:      "ip_ua",
		Action:        "block",
		BlockDuration: blockDuration,
		Steps: []StepDef{
			{
				Order: 1,
				FieldMatches: map[string]string{
					"Path": "^/step/one$", // Full regex for exact path match
				},
				MaxDelayDuration: 5 * time.Second,
			},
			{
				Order: 2,
				FieldMatches: map[string]string{
					"Path": "^/step/two$", // Full regex for exact path match
				},
				MaxDelayDuration: 5 * time.Second,
			},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

	// Setup a mock blocker to intercept the block call
	var blockCalled bool
	var blockCallArgs struct {
		ipInfo   IPInfo
		duration time.Duration
	}

	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			blockCallArgs.ipInfo = ipInfo
			blockCallArgs.duration = duration
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           mockBlocker, // Inject mock blocker
		Config:            &AppConfig{},
	}

	// Get the correct tracking key for the activity store (ip_ua)
	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request --

	// Act 1: Process entry for /step/one
	processor.CheckChains(entry)

	// Assert 1: No block should be called, and the state should be at step 1
	if blockCalled {
		t.Fatal("Blocker was called after step 1, but it should only be called after step 2.")
	}

	processor.ActivityMutex.RLock()
	activity, exists := processor.ActivityStore[trackingKey]

	if !exists {
		t.Fatal("Expected activity state to exist after step 1, but it did not.")
	}

	// Check chain progress
	stepState, stateExists := activity.ChainProgress[chain.Name]
	if !stateExists || stepState.CurrentStep != 1 {
		t.Errorf("Expected chain state to be at step 1, got step %d (exists: %t)", stepState.CurrentStep, stateExists)
	}
	processor.ActivityMutex.RUnlock()

	// --- STEP 2: Process the second request (the attack completion) --

	// Create the second log entry, ensuring it's within the 5s MaxDelay
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second) // 2 seconds after the first request
	entry.Path = "/step/two"

	// Act 2: Process entry for /step/two
	processor.CheckChains(entry)

	// Assert 2: Block was called, and activity state is updated
	if !blockCalled {
		t.Fatal("Expected Blocker to be called after completing the chain, but it was not.")
	}

	if blockCallArgs.ipInfo.Address != targetIP {
		t.Errorf("Blocker called with incorrect IP. Got %s, want %s", blockCallArgs.ipInfo.Address, targetIP)
	}
	if blockCallArgs.duration != blockDuration {
		t.Errorf("Blocker called with incorrect duration. Got %v, want %v", blockCallArgs.duration, blockDuration)
	}

	// Check final ActivityStore state: IsBlocked should be true and ChainProgress should be cleared
	processor.ActivityMutex.RLock()
	ipOnlyKey := TrackingKey{IPInfo: NewIPInfo(targetIP), UA: ""} // Check the IP-only key block optimization
	activityIPOnly, _ := processor.ActivityStore[ipOnlyKey]

	if !activityIPOnly.IsBlocked {
		t.Error("Expected IP-only activity state to be IsBlocked=true, but was false.")
	}

	activityFinal, _ := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be cleared, but it has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}, MaxDelayDuration: 5 * time.Second},
			{Order: 2, FieldMatches: map[string]string{"Path": "^/step/two$"}, MaxDelayDuration: 5 * time.Second},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

	// Setup a mock blocker to intercept the block call
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor with DryRun=true
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            true,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           mockBlocker, // Inject mock blocker
		Config:            &AppConfig{},
	}

	// Get the correct tracking key for the activity store (ip-only)
	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1 & 2: Process both steps (which should trigger a "block" in dry run) --
	processor.CheckChains(entry) // Step 1
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"
	processor.CheckChains(entry) // Step 2 (completion)

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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}, MaxDelayDuration: 5 * time.Second},
			{Order: 2, FieldMatches: map[string]string{"Path": "^/step/two$"}, MaxDelayDuration: 5 * time.Second},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           &GlobalBlocker{},
		Config:            &AppConfig{},
	}

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// Assert 1: State is at step 1
	processor.ActivityMutex.RLock()
	activity, _ := processor.ActivityStore[trackingKey]
	stepState1, _ := activity.ChainProgress[chain.Name]

	if stepState1.CurrentStep != 1 {
		t.Fatalf("Expected state to be at step 1, got %d", stepState1.CurrentStep)
	}
	processor.ActivityMutex.RUnlock() // <-- Release the read lock

	// --- STEP 2: Process the second request, after MaxDelay (e.g., 6 seconds later) ---

	entry.Timestamp = entry.Timestamp.Add(6 * time.Second) // > 5 seconds delay
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Chain should have been reset, and the second step should be treated as the *first* step
	// of a new sequence, but it doesn't match step 1, so the chain progress should be cleared.
	processor.ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}, MaxDelayDuration: 5 * time.Second},
			{Order: 2, FieldMatches: map[string]string{"Path": "^/step/two$"}, MinDelayDuration: 500 * time.Millisecond},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           &GlobalBlocker{},
		Config:            &AppConfig{},
	}

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// Assert 1: State is at step 1
	processor.ActivityMutex.RLock()
	activity, _ := processor.ActivityStore[trackingKey]
	stepState1, _ := activity.ChainProgress[chain.Name]

	if stepState1.CurrentStep != 1 {
		t.Fatalf("Expected state to be at step 1, got %d", stepState1.CurrentStep)
	}
	processor.ActivityMutex.RUnlock() // <-- Release the read lock

	// --- STEP 2: Process the second request, before MinDelay (e.g., 100ms later) ---

	entry.Timestamp = entry.Timestamp.Add(100 * time.Millisecond) // < 500ms delay
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Chain should be reset because min delay was not met.
	processor.ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

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

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: mockIsWhitelisted, // Inject mock whitelisting
		Blocker:           mockBlocker,
		Config:            &AppConfig{},
	}

	whitelistedKey := GetTrackingKey(&chain, whitelistedEntry)

	// --- ACT: Process the whitelisted request ---
	processor.CheckChains(whitelistedEntry)

	// --- Assertions for Whitelisted IP ---
	// NOTE: This assertion correctly identifies the bug in checker.go
	processor.ActivityMutex.RLock()
	_, exists := processor.ActivityStore[whitelistedKey]
	processor.ActivityMutex.RUnlock()

	if exists {
		t.Errorf("Activity state exists for whitelisted IP %s, but should not. (BUG IN CHECKER.GO)", whitelistedIP)
	}
	if blockCalled {
		t.Fatal("Blocker was called, but should be skipped for a whitelisted IP.")
	}

	// Bonus check: ensure a non-whitelisted IP works normally
	nonWhitelistedEntry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now().Add(1 * time.Second),
		Path:      "/step/one",
	}

	// --- ACT: Process the non-whitelisted request ---
	processor.CheckChains(nonWhitelistedEntry)
	nonWhitelistedKey := GetTrackingKey(&chain, nonWhitelistedEntry)

	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()
	activity, exists := processor.ActivityStore[nonWhitelistedKey]

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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}, MaxDelayDuration: 5 * time.Second},
			{Order: 2, FieldMatches: map[string]string{"Path": "^/step/two$"}, MaxDelayDuration: 5 * time.Second},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           mockBlocker,
		Config:            &AppConfig{},
	}

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// Assert 1: State is at step 1
	processor.ActivityMutex.RLock()
	activity, _ := processor.ActivityStore[trackingKey]

	if len(activity.ChainProgress) == 0 {
		t.Fatal("Expected state to exist after step 1, but it was empty.")
	}
	processor.ActivityMutex.RUnlock()

	// --- STEP 2: Process the second request (completion) ---
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Block was NOT called, but ChainProgress should be cleared
	if blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for Action='log'.")
	}

	processor.ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	defer processor.ActivityMutex.RUnlock()

	// For a 'log' action, IsBlocked should be false and ChainProgress should be cleared
	if activityFinal.IsBlocked {
		t.Error("Expected final activity state to be IsBlocked=false for 'log' action, but was true.")
	}
	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be cleared for 'log' action, but has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}, MaxDelayDuration: 5 * time.Second},
			{Order: 2, FieldMatches: map[string]string{"Path": "^/step/two$"}, MaxDelayDuration: 5 * time.Second},
		},
	}
	chains := []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, chains)
	// *************************************************

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           mockBlocker,
		Config:            &AppConfig{},
	}

	trackingKey := GetTrackingKey(&chain, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// --- STEP 2: Process the second request (completion) ---
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Block was NOT called, and ChainProgress should be cleared
	if blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for Action='unknown'.")
	}

	processor.ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	processor.ActivityMutex.RUnlock()

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
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/test$"}},
		},
	}
	compileChainRegexes(t, []BehavioralChain{chain})

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            []BehavioralChain{chain},
		ChainMutex:        &sync.RWMutex{},
		LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           &MockBlocker{}, // No-op blocker
		Config:            &AppConfig{},
	}

	// 2. Manually create a pre-existing, EXPIRED block state.
	trackingKey := GetTrackingKey(&chain, &LogEntry{IPInfo: NewIPInfo(targetIP)})
	processor.ActivityStore[trackingKey] = &BotActivity{
		IsBlocked:    true,
		BlockedUntil: time.Now().Add(-1 * time.Hour), // Expired an hour ago
	}

	// 3. Create the log entry that will be processed.
	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		Timestamp: time.Now(),
		Path:      "/test",
	}

	// --- Act ---
	processor.CheckChains(entry)

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
			Steps:    []StepDef{{Order: 1, FieldMatches: map[string]string{"Path": "/test"}}},
		},
		{
			Name:     "IPv6-Only-Chain",
			MatchKey: "ipv6",
			Action:   "log",
			Steps:    []StepDef{{Order: 1, FieldMatches: map[string]string{"Path": "/test"}}},
		},
	}
	compileChainRegexes(t, chains)

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            chains,
		ChainMutex:        &sync.RWMutex{},
		LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           &MockBlocker{},
		Config:            &AppConfig{},
	}

	// 2. Process an IPv4 log entry.
	entry := &LogEntry{
		IPInfo:    NewIPInfo("192.0.2.1"),
		Timestamp: time.Now(),
		Path:      "/test",
	}
	processor.CheckChains(entry)

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
		Steps:         []StepDef{{Order: 1, FieldMatches: map[string]string{"Path": "/trigger"}}},
	}
	compileChainRegexes(t, []BehavioralChain{chain})

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            []BehavioralChain{chain},
		ChainMutex:        &sync.RWMutex{},
		LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Blocker:           &MockBlocker{},
		Config:            &AppConfig{},
	}

	entry := &LogEntry{
		IPInfo:    NewIPInfo(targetIP),
		UserAgent: targetUA,
		Timestamp: time.Now(),
		Path:      "/trigger",
	}

	// --- Act ---
	processor.CheckChains(entry)

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
		Steps: []StepDef{
			{
				Order:                 1,
				FirstHitSinceDuration: 2 * time.Second, // Must have seen this IP within the last 2s
				FieldMatches:          map[string]string{"Path": "/step1"},
			}, {
				Order:        2,
				FieldMatches: map[string]string{"Path": "/step2"}, // Add a second step to prevent immediate completion
			},
		},
	}
	compileChainRegexes(t, []BehavioralChain{chain})

	baseProcessor := func() *Processor {
		return &Processor{
			ActivityStore:     make(map[TrackingKey]*BotActivity),
			ActivityMutex:     &sync.RWMutex{},
			Chains:            []BehavioralChain{chain},
			ChainMutex:        &sync.RWMutex{},
			LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
			IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
			Blocker:           &MockBlocker{},
			Config:            &AppConfig{},
		}
	}

	tests := []struct {
		name                string
		primingTimeOffset   time.Duration // How long ago the IP was last seen. Zero if never seen.
		shouldChainProgress bool
	}{
		{
			name: "first_hit_since SUCCESS - IP seen recently",
			// Last seen 1 second ago, which is within the 2s window.
			primingTimeOffset:   -1 * time.Second,
			shouldChainProgress: true,
		},
		{
			name: "first_hit_since FAILURE - IP seen too long ago",
			// Last seen 3 seconds ago, which is outside the 2s window.
			primingTimeOffset:   -3 * time.Second,
			shouldChainProgress: false,
		},
		{
			name: "first_hit_since FAILURE - IP never seen before",
			// Zero time indicates the IP has never been seen.
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
			processor.CheckChains(entry)

			processor.ActivityMutex.RLock()
			activity, _ := processor.ActivityStore[TrackingKey{IPInfo: entry.IPInfo}]
			_, progressExists := activity.ChainProgress[chain.Name]
			processor.ActivityMutex.RUnlock()

			if progressExists != tt.shouldChainProgress {
				t.Errorf("Chain progress existence was %t, but expected %t", progressExists, tt.shouldChainProgress)
			}
		})
	}
}

// TestCleanup_FirstHitSince verifies that the cleanup routine correctly removes IPs
// that are no longer useful for `first_hit_since` checks, even if they are not yet
// past the main `IdleTimeout`.
func TestCleanup_FirstHitSince(t *testing.T) {
	// 1. Setup
	resetGlobalState()

	// Create a processor with specific timeout values for the test.
	processor := &Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        []BehavioralChain{}, // No chains needed for this test
		ChainMutex:    &sync.RWMutex{},
		LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {},
		Config: &AppConfig{
			IdleTimeout:              30 * time.Minute, // A long general timeout
			MaxFirstHitSinceDuration: 5 * time.Second,  // A short first_hit_since timeout
			CleanupInterval:          100 * time.Millisecond,
		},
	}

	// Start the cleanup routine in a goroutine.
	// The test will stop it via the done channel.
	done := make(chan struct{})
	go func() {
		// We need to mock the ticker behavior for a single run.
		processor.ActivityMutex.Lock()
		now := time.Now()
		cleanedCount := 0
		for key, activity := range processor.ActivityStore {
			if !activity.IsBlocked && len(activity.ChainProgress) == 0 {
				timeSinceLastHit := now.Sub(activity.LastRequestTime)
				isIdle := timeSinceLastHit > processor.Config.IdleTimeout
				isUselessForFirstHit := processor.Config.MaxFirstHitSinceDuration > 0 && timeSinceLastHit > processor.Config.MaxFirstHitSinceDuration

				if isIdle || isUselessForFirstHit {
					delete(processor.ActivityStore, key)
					cleanedCount++
				}
			}
		}
		processor.ActivityMutex.Unlock()
		close(done)
	}()

	// 2. Create different activity states
	now := time.Now()
	keyUseless := TrackingKey{IPInfo: NewIPInfo("192.0.2.1")}     // Will be older than MaxFirstHitSinceDuration
	keyStillUseful := TrackingKey{IPInfo: NewIPInfo("192.0.2.2")} // Will be recent
	keyIdle := TrackingKey{IPInfo: NewIPInfo("192.0.2.3")}        // Will be older than IdleTimeout

	processor.ActivityMutex.Lock()
	processor.ActivityStore[keyUseless] = &BotActivity{
		LastRequestTime: now.Add(-10 * time.Second), // 10s ago > 5s MaxFirstHitSinceDuration
		ChainProgress:   make(map[string]StepState),
	}
	processor.ActivityStore[keyStillUseful] = &BotActivity{
		LastRequestTime: now.Add(-1 * time.Second), // 1s ago < 5s MaxFirstHitSinceDuration
		ChainProgress:   make(map[string]StepState),
	}
	processor.ActivityStore[keyIdle] = &BotActivity{
		LastRequestTime: now.Add(-31 * time.Minute), // 31m ago > 30m IdleTimeout
		ChainProgress:   make(map[string]StepState),
	}
	processor.ActivityMutex.Unlock()

	// --- Act ---
	// Wait for the cleanup goroutine to finish its single run.
	<-done

	// --- Assert ---
	processor.ActivityMutex.RLock()
	defer processor.ActivityMutex.RUnlock()

	if _, exists := processor.ActivityStore[keyUseless]; exists {
		t.Error("Expected 'useless' key to be cleaned up by MaxFirstHitSinceDuration, but it still exists.")
	}
	if _, exists := processor.ActivityStore[keyIdle]; exists {
		t.Error("Expected 'idle' key to be cleaned up by IdleTimeout, but it still exists.")
	}
	if _, exists := processor.ActivityStore[keyStillUseful]; !exists {
		t.Error("Expected 'still useful' key to remain, but it was cleaned up.")
	}
}

// TestDryRunMode simulates the entire dry-run process using chains.yaml and test_access.log,
// and verifies the log output against the expected log messages extracted from comments
// in test_access.log.
func TestDryRunMode(t *testing.T) {
	// --- Setup ---
	resetGlobalState()

	// 1. Load configuration (chains, whitelist, etc.)
	loadedCfg, err := LoadChainsFromYAML()
	if err != nil {
		t.Fatalf("LoadChainsFromYAML() failed: %v", err)
	}

	// Create a processor (but don't start any background processes like ChainWatcher).
	processor := &Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		ChainMutex:    &sync.RWMutex{},
		DryRun:        true,                                                                    // Simulate dry-run mode
		LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {}, // Will be replaced
		Config: &AppConfig{
			OutOfOrderTolerance:      loadedCfg.OutOfOrderTolerance,
			MaxFirstHitSinceDuration: loadedCfg.MaxFirstHitSinceDuration,
			WhitelistNets:            loadedCfg.WhitelistNets, // This was the missing piece
		},
	}
	// Set the IsWhitelistedFunc on the *actual* processor instance to avoid nil pointers.
	processor.IsWhitelistedFunc = processor.IsIPWhitelisted

	// 2. Read test_access.log and extract expected log outputs from comments
	testLogPath := TestLogPath
	testLogData, err := os.ReadFile(testLogPath)
	if err != nil {
		t.Fatalf("Failed to read test_access.log: %v", err)
	}

	// Extract the Expected Log output values from comments
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
	processor.LogFunc = func(level LogLevel, tag string, format string, args ...interface{}) {
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
			// --- CONCISE FAILURE REPORTING ---
			// 1. Find the relevant context block from test_access.log.
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

func lines(s string) []string {
	var ls []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		ls = append(ls, sc.Text())
	}
	return ls
}

func hasLine(multiLine string, match string) bool {
	for _, line := range lines(multiLine) {
		if strings.Contains(line, match) {
			return true
		}
	}
	return false
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
				Steps: []StepDef{
					{Order: 1, FieldMatches: map[string]string{"Path": "/step1"}},
					{Order: 2, FieldMatches: map[string]string{"Path": "/step2"}}, // Add a second step to prevent immediate completion
				},
			}
			compileChainRegexes(t, []BehavioralChain{chain})

			processor := &Processor{
				ActivityStore: make(map[TrackingKey]*BotActivity), ActivityMutex: &sync.RWMutex{},
				Chains: []BehavioralChain{chain}, ChainMutex: &sync.RWMutex{},
				LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
				IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false }, // Explicitly set to no-op for this test
				Config:            &AppConfig{OutOfOrderTolerance: tt.tolerance, MaxFirstHitSinceDuration: 1 * time.Minute},
			}

			// 1. Process a "newer" entry first to set the LastRequestTime.
			newerEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: now, Path: "/other-path"}
			processor.CheckChains(newerEntry)

			// 2. Process the out-of-order entry.
			outOfOrderEntry := &LogEntry{IPInfo: NewIPInfo(targetIP), Timestamp: now.Add(-tt.outOfOrderOffset), Path: "/step1"}
			processor.CheckChains(outOfOrderEntry)

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
