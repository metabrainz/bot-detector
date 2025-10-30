package main

import (
	"net"
	"regexp"
	"testing"
	"time"
)

// Helper function to reset global state for isolated testing
func resetGlobalState() {
	// Reset runtime state
	ActivityMutex.Lock()
	ActivityStore = make(map[TrackingKey]*BotActivity)
	ActivityMutex.Unlock()

	DryRunActivityMutex.Lock()
	DryRunActivityStore = make(map[TrackingKey]*BotActivity)
	DryRunActivityMutex.Unlock()

	// Reset config-related chains and whitelist
	ChainMutex.Lock()
	Chains = nil
	WhitelistNets = nil
	LastModTime = time.Time{}
	ChainMutex.Unlock()

	// NEW: Reset HAProxy addresses
	HAProxyMutex.Lock()
	HAProxyAddresses = nil
	HAProxyMutex.Unlock()
	
	// NEW: Reset Duration Tables
	DurationTableMutex.Lock()
	DurationToTableName = nil
	BlockTableNameFallback = ""
	DurationTableMutex.Unlock()

	// Reset log level for a clean test environment
	CurrentLogLevel = LevelWarning
	// Reset the exported global variables used for testing flags
	DryRun = false
	LogLevelStr = ""
	PollingIntervalStr = ""
	CleanupIntervalStr = ""
	IdleTimeoutStr = ""
}

// =============================================================================
// I. Configuration and Duration Parsing Tests (ParseDurations)
// =============================================================================

func TestParseDurations(t *testing.T) {
	resetGlobalState()

	// Test Case 1: Valid durations and log level (Dry Run Mode)
	DryRun = true
	LogLevelStr = "info"
	if err := ParseDurations(); err != nil {
		t.Fatalf("Test Case 1 failed: Expected no error in dry-run, got %v", err)
	}
	if CurrentLogLevel != LevelInfo {
		t.Errorf("TC1: Expected log level LevelInfo, got %v", CurrentLogLevel)
	}

	// Test Case 2: Valid durations and log level (Live Mode)
	resetGlobalState()
	DryRun = false
	LogLevelStr = "debug"
	PollingIntervalStr = "10s"
	CleanupIntervalStr = "1m"
	IdleTimeoutStr = "2h"
	if err := ParseDurations(); err != nil {
		t.Fatalf("Test Case 2 failed: Expected no error, got %v", err)
	}
	if PollingInterval != 10*time.Second || CleanupInterval != 1*time.Minute || IdleTimeout != 2*time.Hour {
		t.Errorf("TC2: Duration parsing failed. Got Poll: %v, Cleanup: %v, Idle: %v", PollingInterval, CleanupInterval, IdleTimeout)
	}

	// Test Case 3: Invalid log level
	resetGlobalState()
	LogLevelStr = "unknown"
	if err := ParseDurations(); err == nil {
		t.Fatalf("Test Case 3 failed: Expected error for invalid log level, got nil")
	}

	// Test Case 4: Invalid PollingIntervalStr (Live Mode)
	resetGlobalState()
	DryRun = false
	PollingIntervalStr = "10 sec" // Invalid format
	if err := ParseDurations(); err == nil {
		t.Fatalf("Test Case 4 failed: Expected error for invalid poll-interval, got nil")
	}

	// Test Case 5: Invalid CleanupIntervalStr (Live Mode)
	resetGlobalState()
	DryRun = false
	CleanupIntervalStr = "1 min" // Invalid format
	if err := ParseDurations(); err == nil {
		t.Fatalf("Test Case 5 failed: Expected error for invalid cleanup-interval, got nil")
	}

	// Test Case 6: Invalid IdleTimeoutStr (Live Mode)
	resetGlobalState()
	DryRun = false
	IdleTimeoutStr = "2 hours" // Invalid format
	if err := ParseDurations(); err == nil {
		t.Fatalf("Test Case 6 failed: Expected error for invalid idle-timeout, got nil")
	}
}

// =============================================================================
// II. Core Log Parsing Tests (ParseLogLine)
// =============================================================================

func TestLogLineParsing(t *testing.T) {
	resetGlobalState()
	// Test cases for the combined log format parsing logic

	// Valid log line (Apache Combined/HAProxy common style)
	logLine1 := `192.168.1.1 - - [29/Oct/2025:10:00:00 +0100] "GET /index.html HTTP/1.1" 200 1234 "-" "TestAgent/1.0"`
	entry1, err1 := ParseLogLine(logLine1)
	if err1 != nil {
		t.Fatalf("TC1 failed: Expected no error, got %v", err1)
	}
	if entry1.IP != "192.168.1.1" || entry1.Method != "GET" || entry1.Path != "/index.html" || entry1.StatusCode != 200 || entry1.UserAgent != "TestAgent/1.0" {
		t.Errorf("TC1 parsing mismatch: Got IP: %s, Method: %s, Path: %s, Status: %d, UA: %s", entry1.IP, entry1.Method, entry1.Path, entry1.StatusCode, entry1.UserAgent)
	}

	// Malformed log line (Too few parts)
	logLine2 := `192.168.1.1 - - [29/Oct/2025:10:00:00 +0100] "GET /index.html HTTP/1.1"`
	_, err2 := ParseLogLine(logLine2)
	if err2 == nil {
		t.Fatalf("TC2 failed: Expected error for malformed line, got nil")
	}

	// Valid log line with missing Referrer and complex UserAgent
	logLine3 := `1.2.3.4 - - [30/Oct/2025:14:30:00 +0100] "POST /api/data?q=1 HTTP/2.0" 404 10 "-" "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"`
	entry3, err3 := ParseLogLine(logLine3)
	if err3 != nil {
		t.Fatalf("TC3 failed: Expected no error, got %v", err3)
	}
	if entry3.Referrer != "-" || entry3.Method != "POST" || entry3.Protocol != "HTTP/2.0" {
		t.Errorf("TC3 parsing mismatch: Got Referrer: %s, Method: %s, Protocol: %s", entry3.Referrer, entry3.Method, entry3.Protocol)
	}

	// Invalid date format test removed as it relies on log output not assertion error.
}

// =============================================================================
// III. Utility and State Management Tests (IP, Whitelist, Tracking, Cleanup)
// =============================================================================

func TestIPWhitelistCheck(t *testing.T) {
	resetGlobalState()
	// Set up a fake whitelist
	ChainMutex.Lock()
	_, netA, _ := net.ParseCIDR("192.168.0.0/24")
	_, netB, _ := net.ParseCIDR("10.0.0.0/8")
	WhitelistNets = []*net.IPNet{netA, netB}
	ChainMutex.Unlock()

	// TC1: Whitelisted IP
	if !IsIPWhitelisted("192.168.0.50") {
		t.Errorf("TC1 failed: Expected 192.168.0.50 to be whitelisted")
	}

	// TC2: Non-whitelisted IP
	if IsIPWhitelisted("172.16.0.1") {
		t.Errorf("TC2 failed: Expected 172.16.0.1 not to be whitelisted")
	}

	// TC3: CIDR boundary IP
	if !IsIPWhitelisted("10.255.255.255") {
		t.Errorf("TC3 failed: Expected 10.255.255.255 to be whitelisted")
	}
}

func TestTrackingKeyLogic(t *testing.T) {
	resetGlobalState()
	chainIP := BehavioralChain{MatchKey: "ip"}
	chainIPUA := BehavioralChain{MatchKey: "ip_ua"}
	entry := &LogEntry{IP: "1.1.1.1", UserAgent: "TestUA"}

	// TC1: MatchKey "ip"
	key1 := GetTrackingKey(&chainIP, entry)
	expectedKey1 := TrackingKey{IP: "1.1.1.1", UA: ""}
	if key1 != expectedKey1 {
		t.Errorf("TC1 failed: Expected key %v, got %v", expectedKey1, key1)
	}

	// TC2: MatchKey "ip_ua"
	key2 := GetTrackingKey(&chainIPUA, entry)
	expectedKey2 := TrackingKey{IP: "1.1.1.1", UA: "TestUA"}
	if key2 != expectedKey2 {
		t.Errorf("TC2 failed: Expected key %v, got %v", expectedKey2, key2)
	}

	// TC3: Invalid IP and "ip" key
	invalidEntry := &LogEntry{IP: "invalid-ip", UserAgent: "TestUA"}
	key3 := GetTrackingKey(&chainIP, invalidEntry)
	expectedKey3 := TrackingKey{}
	if key3 != expectedKey3 {
		t.Errorf("TC3 failed: Expected empty key, got %v", key3)
	}
}

// =============================================================================
// IV. Chain Progression and Action Tests (CheckChains)
// =============================================================================

func TestChainProgressAndAction(t *testing.T) {
	resetGlobalState()

	// Set DryRun mode to use the isolated DryRunActivityStore
	DryRun = true
	CurrentLogLevel = LevelDebug

	// 1. Setup a minimal chain:
	// Step 1: Path: /step1, MaxDelay: 5s, MinDelay: 0s
	// Step 2: Path: /step2, MaxDelay: 0s, MinDelay: 1s <-- MIN DELAY MOVED TO STEP 2
	// Action: block, Duration: 5m
	chain := BehavioralChain{
		Name:          "TestChain",
		Action:        "block",
		BlockDuration: 5 * time.Minute,
		MatchKey:      "ip",
		Steps: []StepDef{
			{
				Order: 1,
				FieldMatches: map[string]string{
					"Path": "^/step1$",
				},
				MaxDelayDuration: 5 * time.Second,
				MinDelayDuration: 0, // FIXED: Min Delay is now 0 on the first step
				CompiledRegexes: map[string]*regexp.Regexp{
					"Path": regexp.MustCompile("^/step1$"),
				},
			},
			{
				Order: 2,
				FieldMatches: map[string]string{
					"Path": "^/step2$",
				},
				MaxDelayDuration: 0,
				MinDelayDuration: 1 * time.Second, // FIXED: Min Delay is now 1s on the step being checked
				CompiledRegexes: map[string]*regexp.Regexp{
					"Path": regexp.MustCompile("^/step2$"),
				},
			},
		},
	}
	ChainMutex.Lock()
	Chains = []BehavioralChain{chain}
	ChainMutex.Unlock()

	// Initial setup
	ip := "1.2.3.4"
	key := TrackingKey{IP: ip}
	t1 := time.Date(2025, time.October, 29, 10, 0, 0, 0, time.UTC)

	// --- 1. Initial Match (Match step 1) ---
	entry1 := &LogEntry{IP: ip, Path: "/step1", Protocol: "HTTP/1.1", Timestamp: t1, Method: "GET"}
	CheckChains(entry1)

	DryRunActivityMutex.Lock()
	activity := DryRunActivityStore[key]
	state := activity.ChainProgress["TestChain"]
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t1) {
		t.Fatalf("TC1 failed: Expected step 1, time %v. Got step %d, time %v", t1, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 2. Min Delay Check (Match step 2, but fail Min Delay) ---
	// Expected behavior: Min Delay check fails (500ms < 1s), execution continues to the next chain, 
	// state remains at step 1 (CurrentStep: 1, LastMatchTime: t1).
	t2 := t1.Add(500 * time.Millisecond) // < 1s min delay
	entry2Short := &LogEntry{IP: ip, Path: "/step2", Protocol: "HTTP/1.1", Timestamp: t2, Method: "GET"}
	CheckChains(entry2Short)

	DryRunActivityMutex.Lock()
	state = DryRunActivityStore[key].ChainProgress["TestChain"]
	// Min delay failed: State must remain at step 1. LastMatchTime must be t1.
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t1) {
		t.Fatalf("TC2 failed: Expected step 1 and LastMatchTime %v (t1) after Min Delay fail. Got step %d, time %v", t1, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 3. Max Delay Check (Timeout, resets progress to 0) ---
	t3 := t1.Add(6 * time.Second) // > 5s max delay (from step 1)
	entry3Reset := &LogEntry{IP: ip, Path: "/step1", Protocol: "HTTP/1.1", Timestamp: t3, Method: "GET"}
	CheckChains(entry3Reset) // Match step 1 again.

	DryRunActivityMutex.Lock()
	state = DryRunActivityStore[key].ChainProgress["TestChain"]
	// Since t3 > t1 + 5s (MaxDelay), the state was reset, and then successfully stepped to 1 again.
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t3) {
		t.Fatalf("TC3 failed: Expected reset then step 1, time %v. Got step %d, time %v", t3, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 4. Chain Completion (Block Action) ---
	t4 := t3.Add(2 * time.Second) // 2s delay (> 1s min delay)
	entry4Complete := &LogEntry{IP: ip, Path: "/step2", Protocol: "HTTP/2.0", Timestamp: t4, Method: "GET"}
	CheckChains(entry4Complete)

	DryRunActivityMutex.Lock()
	activity = DryRunActivityStore[key]
	state = activity.ChainProgress["TestChain"]
	if state.CurrentStep != 0 {
		t.Fatalf("TC4 failed: Expected step 0 (Chain complete and reset), got %d", state.CurrentStep)
	}
	// Verify local block state update after action
	ipOnlyKey := TrackingKey{IP: entry4Complete.IP, UA: ""}
	ipActivity := DryRunActivityStore[ipOnlyKey]
	expectedBlockUntil := t4.Add(5 * time.Minute)
	if !ipActivity.IsBlocked || !ipActivity.BlockedUntil.Equal(expectedBlockUntil) {
		t.Fatalf("TC4 failed: Expected IsBlocked=true and BlockedUntil=%v. Got IsBlocked=%t, BlockedUntil=%v", expectedBlockUntil, ipActivity.IsBlocked, ipActivity.BlockedUntil)
	}
	DryRunActivityMutex.Unlock()

	// --- 5. Blocked Skip Check ---
	t5 := t4.Add(1 * time.Minute) // Still within 5m block window
	entry5Blocked := &LogEntry{IP: ip, Path: "/anywhere", Protocol: "HTTP/1.1", Timestamp: t5, Method: "GET"}
	CheckChains(entry5Blocked)
	
	DryRunActivityMutex.Lock()
	// Chain progress should remain reset/empty, as the activity was skipped before chain check.
	state, exists := DryRunActivityStore[key].ChainProgress["TestChain"]
	if exists && state.CurrentStep != 0 {
		t.Fatalf("TC5 failed: Expected state to be skipped or reset (step 0), got %d", state.CurrentStep)
	}
	DryRunActivityMutex.Unlock()
}
