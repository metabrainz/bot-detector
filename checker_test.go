package main

import (
	"regexp"
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
		IP:        targetIP,
		UserAgent: "TestAgent",
		IPVersion: VersionIPv4,
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
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	// Setup a mock blocker to intercept the block call
	var blockCalled bool
	var blockCallArgs struct {
		ip       string
		version  IPVersion
		duration time.Duration
	}

	mockBlocker := &MockBlocker{
		BlockFunc: func(ip string, version IPVersion, duration time.Duration) error {
			blockCalled = true
			blockCallArgs.ip = ip
			blockCallArgs.version = version
			blockCallArgs.duration = duration
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           mockBlocker, // Inject mock blocker
	}

	// Get the correct tracking key for the activity store (ip_ua)
	trackingKey := GetTrackingKeyFromLogEntry(Chains, entry)

	// --- STEP 1: Process the first request --

	// Act 1: Process entry for /step/one
	processor.CheckChains(entry)

	// Assert 1: No block should be called, and the state should be at step 1
	if blockCalled {
		t.Fatal("Blocker was called after step 1, but it should only be called after step 2.")
	}

	ActivityMutex.RLock()
	activity, exists := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

	if !exists {
		t.Fatal("Expected activity state to exist after step 1, but it did not.")
	}

	// Check chain progress
	stepState, stateExists := activity.ChainProgress[chain.Name]
	if !stateExists || stepState.CurrentStep != 1 {
		t.Errorf("Expected chain state to be at step 1, got step %d (exists: %t)", stepState.CurrentStep, stateExists)
	}

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

	if blockCallArgs.ip != targetIP {
		t.Errorf("Blocker called with incorrect IP. Got %s, want %s", blockCallArgs.ip, targetIP)
	}
	if blockCallArgs.duration != blockDuration {
		t.Errorf("Blocker called with incorrect duration. Got %v, want %v", blockCallArgs.duration, blockDuration)
	}

	// Check final ActivityStore state: IsBlocked should be true and ChainProgress should be cleared
	ActivityMutex.RLock()
	ipOnlyKey := TrackingKey{IP: targetIP, UA: ""} // Check the IP-only key block optimization
	activityIPOnly, _ := processor.ActivityStore[ipOnlyKey]
	ActivityMutex.RUnlock()

	if !activityIPOnly.IsBlocked {
		t.Error("Expected IP-only activity state to be IsBlocked=true, but was false.")
	}

	ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

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
		IP:        targetIP,
		UserAgent: "TestAgent",
		IPVersion: VersionIPv4,
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
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	// Setup a mock blocker to intercept the block call
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ip string, version IPVersion, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor with DryRun=true
	processor := &Processor{
		ActivityStore:     ActivityStore, // Use global store for DryRun
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            true,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           mockBlocker, // Inject mock blocker
	}
	DryRunActivityStore = make(map[TrackingKey]*BotActivity) // Initialize the dry-run store

	// Get the correct tracking key for the activity store (ip-only)
	trackingKey := GetTrackingKeyFromLogEntry(Chains, entry)

	// --- STEP 1 & 2: Process both steps (which should trigger a "block" in dry run) --
	processor.CheckChains(entry) // Step 1
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"
	processor.CheckChains(entry) // Step 2 (completion)

	// Assertions
	if blockCalled {
		t.Fatal("Blocker was called, but should be skipped in DryRun mode.")
	}

	// The DryRunActivityStore should be updated, but the main ActivityStore should be empty
	ActivityMutex.RLock()
	if _, exists := ActivityStore[trackingKey]; exists {
		t.Error("Main ActivityStore should be empty in DryRun mode.")
	}
	ActivityMutex.RUnlock()

	DryRunActivityMutex.RLock()
	activityFinal, exists := DryRunActivityStore[trackingKey]
	DryRunActivityMutex.RUnlock()

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
		IP:        targetIP,
		UserAgent: "TestAgent",
		IPVersion: VersionIPv4,
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
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           &MockBlocker{},
	}

	trackingKey := GetTrackingKeyFromLogEntry(Chains, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// Assert 1: State is at step 1
	ActivityMutex.RLock()
	activity, _ := processor.ActivityStore[trackingKey]
	stepState1, _ := activity.ChainProgress[chain.Name]
	ActivityMutex.RUnlock()

	if stepState1.CurrentStep != 1 {
		t.Fatalf("Expected state to be at step 1, got %d", stepState1.CurrentStep)
	}

	// --- STEP 2: Process the second request, after MaxDelay (e.g., 6 seconds later) ---

	entry.Timestamp = entry.Timestamp.Add(6 * time.Second) // > 5 seconds delay
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Chain should have been reset, and the second step should be treated as the *first* step
	// of a new sequence, but it doesn't match step 1, so the chain progress should be cleared.
	ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

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
		IP:        targetIP,
		UserAgent: "TestAgent",
		IPVersion: VersionIPv4,
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
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           &MockBlocker{},
	}

	trackingKey := GetTrackingKeyFromLogEntry(Chains, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// Assert 1: State is at step 1
	ActivityMutex.RLock()
	activity, _ := processor.ActivityStore[trackingKey]
	stepState1, _ := activity.ChainProgress[chain.Name]
	ActivityMutex.RUnlock()

	if stepState1.CurrentStep != 1 {
		t.Fatalf("Expected state to be at step 1, got %d", stepState1.CurrentStep)
	}

	// --- STEP 2: Process the second request, before MinDelay (e.g., 100ms later) ---

	entry.Timestamp = entry.Timestamp.Add(100 * time.Millisecond) // < 500ms delay
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Chain should be reset because min delay was not met.
	ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

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
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	// Log entry template for whitelisted IP
	whitelistedEntry := &LogEntry{
		IP:        whitelistedIP,
		UserAgent: "TestAgent",
		IPVersion: VersionIPv4,
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Setup whitelisting using a mock function for IsIPWhitelisted
	mockIsWhitelisted := func(ip string) bool {
		return ip == whitelistedIP
	}

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ip string, version IPVersion, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: mockIsWhitelisted, // Inject mock whitelisting
		Blocker:           mockBlocker,
	}

	whitelistedKey := GetTrackingKeyFromLogEntry(Chains, whitelistedEntry)

	// --- ACT: Process the whitelisted request ---
	processor.CheckChains(whitelistedEntry)

	// --- Assertions for Whitelisted IP ---
	// NOTE: This assertion correctly identifies the bug in checker.go
	ActivityMutex.RLock()
	_, exists := processor.ActivityStore[whitelistedKey]
	ActivityMutex.RUnlock()

	if exists {
		t.Errorf("Activity state exists for whitelisted IP %s, but should not. (BUG IN CHECKER.GO)", whitelistedIP)
	}
	if blockCalled {
		t.Fatal("Blocker was called, but should be skipped for a whitelisted IP.")
	}

	// Bonus check: ensure a non-whitelisted IP works normally
	nonWhitelistedEntry := &LogEntry{
		IP:        targetIP,
		IPVersion: VersionIPv4,
		Timestamp: time.Now().Add(1 * time.Second),
		Path:      "/step/one",
	}

	// --- ACT: Process the non-whitelisted request ---
	processor.CheckChains(nonWhitelistedEntry)
	nonWhitelistedKey := GetTrackingKeyFromLogEntry(Chains, nonWhitelistedEntry)

	// --- Assertions for Non-Whitelisted IP ---
	ActivityMutex.RLock()
	activity, exists := processor.ActivityStore[nonWhitelistedKey]
	ActivityMutex.RUnlock()

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
		IP:        targetIP,
		IPVersion: VersionIPv4,
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
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ip string, version IPVersion, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           mockBlocker,
	}

	trackingKey := GetTrackingKeyFromLogEntry(Chains, entry)

	// --- STEP 1: Process the first request ---
	processor.CheckChains(entry)

	// Assert 1: State is at step 1
	ActivityMutex.RLock()
	activity, _ := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

	if len(activity.ChainProgress) == 0 {
		t.Fatal("Expected state to exist after step 1, but it was empty.")
	}

	// --- STEP 2: Process the second request (completion) ---
	entry.Timestamp = entry.Timestamp.Add(2 * time.Second)
	entry.Path = "/step/two"

	processor.CheckChains(entry)

	// Assert 2: Block was NOT called, but ChainProgress should be cleared
	if blockCalled {
		t.Fatal("Blocker was called, but should have been skipped for Action='log'.")
	}

	ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

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

	// Log entry template
	entry := &LogEntry{
		IP:        targetIP,
		IPVersion: VersionIPv4,
		Timestamp: time.Now(),
		Path:      "/step/one",
	}

	// Define a two-step chain with an unrecognized action
	chain := BehavioralChain{
		Name:          "UnknownActionTestChain",
		MatchKey:      "ip",
		Action:        "unknown", // ACTION: unknown
		BlockDuration: 1 * time.Minute,
		Steps: []StepDef{
			{Order: 1, FieldMatches: map[string]string{"Path": "^/step/one$"}, MaxDelayDuration: 5 * time.Second},
			{Order: 2, FieldMatches: map[string]string{"Path": "^/step/two$"}, MaxDelayDuration: 5 * time.Second},
		},
	}
	Chains = []BehavioralChain{chain}

	// *** FIX: Compile regexes after chain definition ***
	compileChainRegexes(t, Chains)
	// *************************************************

	// Setup a mock blocker to ensure it's not called
	var blockCalled bool
	mockBlocker := &MockBlocker{
		BlockFunc: func(ip string, version IPVersion, duration time.Duration) error {
			blockCalled = true
			return nil
		},
	}

	// Create the processor
	processor := &Processor{
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &ActivityMutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            false,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           mockBlocker,
	}

	trackingKey := GetTrackingKeyFromLogEntry(Chains, entry)

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

	ActivityMutex.RLock()
	activityFinal, _ := processor.ActivityStore[trackingKey]
	ActivityMutex.RUnlock()

	// For an 'unknown' action, IsBlocked should be false and ChainProgress should be cleared
	if activityFinal.IsBlocked {
		t.Error("Expected final activity state to be IsBlocked=false for 'unknown' action, but was true.")
	}
	if len(activityFinal.ChainProgress) != 0 {
		t.Errorf("Expected ChainProgress to be cleared for 'unknown' action, but has %d entries: %v", len(activityFinal.ChainProgress), activityFinal.ChainProgress)
	}
}
