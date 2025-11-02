package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
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

// --- HAProxy Mocking Setup ---

// MockHAProxyServer simulates a single HAProxy instance socket.
// It is modified to:
// 1. Loop and handle concurrent connections (for UnblockIP).
// 2. Send a newline response when 'success' is true (to prevent client read timeout).
// 3. Trim the newline from the recorded command (to match the test assertion).
func MockHAProxyServer(t *testing.T, addr string, commandsReceived *[]string, wg *sync.WaitGroup, success bool) net.Listener {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to start mock HAProxy server on %s: %v", addr, err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		// FIX 1: Loop to accept multiple concurrent connections (required for UnblockIP)
		for {
			conn, err := listener.Accept()
			if err != nil {
				// This is the expected exit for the server when the test closes the listener
				return
			}

			// Handle the connection in a new goroutine to process concurrently
			go func() {
				defer conn.Close()

				reader := bufio.NewReader(conn)

				// Read the command
				command, err := reader.ReadString('\n')
				if err != nil && err != io.EOF {
					// Log the error but don't fail the test
					t.Logf("Mock server %s command read error: %v", addr, err)
					return
				}

				// Record the command
				// FIX 2: Trim the newline before recording to match the assertion format
				trimmedCommand := strings.TrimSpace(command)

				if trimmedCommand != "" {
					// This is thread-safe due to the wait group in the calling function,
					// which ensures all commands are received before checking assertions.
					*commandsReceived = append(*commandsReceived, trimmedCommand)
				}

				// FIX 3: Send a response for successful commands to prevent client read timeout
				if success {
					// Send a simple newline response to unblock the client's ReadString('\n')
					conn.Write([]byte("\n"))
				}
				// If success is false, we intentionally don't respond to simulate a failure/timeout.

			}() // End connection goroutine
		}
	}()

	return listener
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
		t.Errorf("TC1 failed: Expected CurrentLogLevel to be info, got %v", CurrentLogLevel)
	}

	// Test Case 2: Valid durations and log level (Live Mode)
	resetGlobalState() // Reset for the next case
	DryRun = false
	LogLevelStr = "debug"
	PollingIntervalStr = "10s"
	CleanupIntervalStr = "1m"
	IdleTimeoutStr = "30m"
	if err := ParseDurations(); err != nil {
		t.Fatalf("Test Case 2 failed: Expected no error, got %v", err)
	}
	if CurrentLogLevel != LevelDebug || PollingInterval != 10*time.Second || CleanupInterval != 1*time.Minute || IdleTimeout != 30*time.Minute {
		t.Errorf("TC2 failed: Mismatch in parsed values. Expected: debug, 10s, 1m, 30m. Got: %v, %v, %v, %v", CurrentLogLevel, PollingInterval, CleanupInterval, IdleTimeout)
	}

	// Test Case 3: Invalid log level
	resetGlobalState()
	LogLevelStr = "invalid"
	if err := ParseDurations(); err == nil {
		t.Errorf("TC3 failed: Expected error for invalid log level, got nil")
	}

	// Test Case 4: Invalid duration
	// FIX: resetGlobalState() must be called BEFORE setting test variables
	resetGlobalState()
	LogLevelStr = "info"
	PollingIntervalStr = "10z"
	if err := ParseDurations(); err == nil || !strings.Contains(err.Error(), "invalid poll-interval format") {
		t.Errorf("TC4 failed: Expected error for invalid poll-interval, got %v", err)
	}
}

// =============================================================================
// II. Configuration Loading Tests (LoadChainsFromYAML)
// =============================================================================

// Mock YAML file content for successful loading
const mockYAMLContent = `
version: 1.0
haproxy_addresses:
    - 127.0.0.1:9001
    - 127.0.0.1:9002

whitelist_cidrs:
    - 192.168.1.0/24
    - 10.0.0.0/8

duration_tables:
    5m: table_5m
    1h: table_1h

chains:
    - name: TestChain
      match_key: ip_ua
      action: block
      block_duration: 5m
      steps:
          - order: 1
            field_matches:
                Path: /step1
            max_delay: 5s
            min_delay: 1s
          - order: 2
            field_matches:
                Path: /step2
                Method: GET
            max_delay: 3s
            min_delay: 1s
    - name: LogOnlyChain
      match_key: ip
      action: log
      block_duration: 1h # Should be ignored for 'log' action
      steps:
          - order: 1
            field_matches:
                Path: /test_log
`

// Mock YAML file content for error case (invalid CIDR)
const mockYAMLCIDRError = `
version: 1.0
whitelist_cidrs:
    - 192.168.1.0/24
    - invalid-cidr
`

// Mock YAML file content for error case (invalid duration)
const mockYAMLDurationError = `
version: 1.0
chains:
    - name: TestChain
      match_key: ip
      action: block
      block_duration: 5z
      steps:
          - order: 1
            field_matches:
                Path: /step1
`

// Mock YAML file content for error case (invalid regex)
const mockYAMLRegexError = `
version: 1.0
chains:
    - name: TestChain
      match_key: ip
      action: block
      block_duration: 5m
      steps:
          - order: 1
            field_matches:
                Path: [
`

// Mock YAML for config.go parser
const mockYAMLConfigGo = `
version: 1.0
haproxy_addresses:
    - 127.0.0.1:9001
    - 127.0.0.1:9002
duration_tables:
    5m: table_5m
    1h: table_1h
whitelist_cidrs:
    - 10.0.0.0/8
chains:
    - name: TestChain
      match_key: ip
      action: block
      block_duration: 5m
      steps:
          - order: 1
            max_delay: 5s
            min_delay: 1s
            field_matches:
                Path: "^/step1$"
          - order: 2
            max_delay: 5s
            min_delay: 1s
            field_matches:
                Path: "^/step2$"
`

func TestLoadChainsFromYAML(t *testing.T) {
	resetGlobalState()

	// Create a temporary YAML file
	tmpFile, err := os.CreateTemp("", "chains-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := tmpFile.WriteString(mockYAMLConfigGo); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	// Set the global path variable to the temp file
	YAMLFilePath = tmpFile.Name()

	// Load the chains
	chains, err := LoadChainsFromYAML()
	if err != nil {
		t.Fatalf("Failed to load chains: %v", err)
	}

	// 1. Basic checks
	if len(chains) != 1 || chains[0].Name != "TestChain" {
		t.Errorf("TC1 failed: Expected 1 chain named 'TestChain', got %d", len(chains))
	}

	// 2. HAProxy config check
	HAProxyMutex.RLock()
	if len(HAProxyAddresses) != 2 || HAProxyAddresses[0] != "127.0.0.1:9001" {
		t.Errorf("TC2 failed: HAProxyAddresses mismatch. Got %v", HAProxyAddresses)
	}
	HAProxyMutex.RUnlock()

	// 3. Duration Table check
	DurationTableMutex.RLock()
	if len(DurationToTableName) != 2 || DurationToTableName[5*time.Minute] != "table_5m" || DurationToTableName[1*time.Hour] != "table_1h" || BlockTableNameFallback != "table_1h" {
		t.Errorf("TC3 failed: DurationTable mismatch. Got %v, Fallback: %s", DurationToTableName, BlockTableNameFallback)
	}
	DurationTableMutex.RUnlock()

	// 4. Step details check (regex and duration parsing)
	if len(chains[0].Steps) != 2 {
		t.Fatalf("TC4 failed: Expected 2 steps, got %d", len(chains[0].Steps))
	}
	step1 := chains[0].Steps[0]
	if step1.MaxDelayDuration != 5*time.Second || step1.MinDelayDuration != 1*time.Second {
		t.Errorf("TC4 failed: Step 1 duration mismatch. Got Max: %v, Min: %v", step1.MaxDelayDuration, step1.MinDelayDuration)
	}
	if re, ok := step1.CompiledRegexes["Path"]; !ok || re.String() != "^/step1$" {
		t.Errorf("TC4 failed: Step 1 regex mismatch. Got %v", re)
	}
}

// =============================================================================
// III. HAProxy Communication Tests (BlockIPForDuration, UnblockIP)
// =============================================================================

func TestHAProxyExecution(t *testing.T) {
	resetGlobalState()
	DryRun = false // Must be false to test live HAProxy calls

	// Setup HAProxy Addresses and Duration Tables (mimicking config load)
	HAProxyMutex.Lock()
	HAProxyAddresses = []string{"127.0.0.1:9001", "127.0.0.1:9002"}
	HAProxyMutex.Unlock()

	DurationTableMutex.Lock()
	DurationToTableName = map[time.Duration]string{
		5 * time.Minute: "table_5m",
		1 * time.Hour:   "table_1h",
	}
	BlockTableNameFallback = "table_1h"
	DurationTableMutex.Unlock()

	// Mock server setup
	var wg sync.WaitGroup
	var commandsReceived1, commandsReceived2 []string

	// Start a successful mock server
	listener1 := MockHAProxyServer(t, "127.0.0.1:9001", &commandsReceived1, &wg, true)
	defer listener1.Close()
	// Start a successful mock server
	listener2 := MockHAProxyServer(t, "127.0.0.1:9002", &commandsReceived2, &wg, true)
	defer listener2.Close()

	ipToBlock := "1.2.3.4"
	duration := 1 * time.Hour

	// --- Test Case 1: BlockIPForDuration (Successful) ---
	if err := BlockIP(ipToBlock, duration); err != nil {
		t.Fatalf("TC1 failed: Expected BlockIP to succeed, got %v", err)
	}

	// We must close the listeners to stop the mock server's Accept loop
	// This allows the WaitGroup to complete.
	listener1.Close()
	listener2.Close()
	wg.Wait() // Wait for mock servers to shut down

	expectedCommand := "set table table_1h key 1.2.3.4 data 1"

	if len(commandsReceived1) != 1 || commandsReceived1[0] != expectedCommand {
		t.Errorf("TC1 (9001): Expected command '%s', got '%v'", expectedCommand, commandsReceived1)
	}
	if len(commandsReceived2) != 1 || commandsReceived2[0] != expectedCommand {
		t.Errorf("TC1 (9002): Expected command '%s', got '%v'", expectedCommand, commandsReceived2)
	}

	// --- Test Case 2: UnblockIP (Successful) ---
	// Reset received commands for unblock test
	commandsReceived1 = nil
	commandsReceived2 = nil

	// Restart the mock servers for the next test
	listener1 = MockHAProxyServer(t, "127.0.0.1:9001", &commandsReceived1, &wg, true)
	defer listener1.Close()
	listener2 = MockHAProxyServer(t, "127.0.0.1:9002", &commandsReceived2, &wg, true)
	defer listener2.Close()

	if err := UnblockIP(ipToBlock); err != nil {
		t.Fatalf("TC2 failed: Expected UnblockIP to succeed, got %v", err)
	}

	// Close listeners and wait for goroutines
	listener1.Close()
	listener2.Close()
	wg.Wait()

	expectedUnblockCommands := map[string]struct{}{
		"set table table_5m key 1.2.3.4 remove": {},
		"set table table_1h key 1.2.3.4 remove": {},
	}

	// Check commands on server 1
	if len(commandsReceived1) != 2 {
		t.Errorf("TC2 (9001): Expected 2 commands, got %d. Commands: %v", len(commandsReceived1), commandsReceived1)
	}
	for _, cmd := range commandsReceived1 {
		if _, ok := expectedUnblockCommands[cmd]; !ok {
			t.Errorf("TC2 (9001): Received unexpected command: %s", cmd)
		}
	}

	// Check commands on server 2
	if len(commandsReceived2) != 2 {
		t.Errorf("TC2 (9002): Expected 2 commands, got %d. Commands: %v", len(commandsReceived2), commandsReceived2)
	}
	for _, cmd := range commandsReceived2 {
		if _, ok := expectedUnblockCommands[cmd]; !ok {
			t.Errorf("TC2 (9002): Received unexpected command: %s", cmd)
		}
	}
}

// =============================================================================
// IV. Whitelist Cleanup Test (CheckAndRemoveWhitelistedBlocks)
// =============================================================================

func TestCheckAndRemoveWhitelistedBlocks(t *testing.T) {
	resetGlobalState()
	DryRun = false

	// Setup config (mimicking a config load)
	HAProxyMutex.Lock()
	HAProxyAddresses = []string{"127.0.0.1:9001"}
	HAProxyMutex.Unlock()

	DurationTableMutex.Lock()
	DurationToTableName = map[time.Duration]string{
		5 * time.Minute: "table_5m",
		1 * time.Hour:   "table_1h",
	}
	BlockTableNameFallback = "table_1h"
	DurationTableMutex.Unlock()

	// Initial Whitelist (protected by ChainMutex)
	ChainMutex.Lock()
	newWhitelistNets := make([]*net.IPNet, 0)
	_, ipNet, _ := net.ParseCIDR("192.168.1.0/24")
	newWhitelistNets = append(newWhitelistNets, ipNet)
	WhitelistNets = newWhitelistNets
	ChainMutex.Unlock()

	// Mock HAProxy Server
	var wg sync.WaitGroup
	var commandsReceived []string
	listener := MockHAProxyServer(t, "127.0.0.1:9001", &commandsReceived, &wg, true)
	defer listener.Close()

	// --- 1. Setup Blocked IPs in ActivityStore ---
	t1 := time.Now()
	ip1Blocked := "10.0.0.5"                // Not in initial whitelist
	ip2WhitelistedBlocked := "192.168.1.10" // IS in initial whitelist (should be unblocked immediately on config load)
	ip3Blocked := "1.1.1.1"                 // Not in any whitelist

	ActivityMutex.Lock()
	// Block ip1Blocked (will not be in whitelist)
	activity1 := GetOrCreateActivityUnsafe(ActivityStore, TrackingKey{IP: ip1Blocked})
	activity1.IsBlocked = true
	activity1.BlockedUntil = t1.Add(1 * time.Hour)
	// Block ip2WhitelistedBlocked (will be in whitelist)
	activity2 := GetOrCreateActivityUnsafe(ActivityStore, TrackingKey{IP: ip2WhitelistedBlocked})
	activity2.IsBlocked = true
	activity2.BlockedUntil = t1.Add(1 * time.Hour)
	// Block ip3Blocked (not in whitelist, remains blocked)
	activity3 := GetOrCreateActivityUnsafe(ActivityStore, TrackingKey{IP: ip3Blocked})
	activity3.IsBlocked = true
	activity3.BlockedUntil = t1.Add(1 * time.Hour)
	ActivityMutex.Unlock()

	// --- 2. Change Whitelist to include ip1Blocked (and keep ip2WhitelistedBlocked) ---
	ChainMutex.Lock()
	newWhitelistNets = make([]*net.IPNet, 0)
	_, ipNet1, _ := net.ParseCIDR("10.0.0.0/8")     // NEWLY whitelists ip1Blocked
	_, ipNet2, _ := net.ParseCIDR("192.168.1.0/24") // Keeps ip2WhitelistedBlocked whitelisted
	newWhitelistNets = append(newWhitelistNets, ipNet1, ipNet2)
	WhitelistNets = newWhitelistNets
	ChainMutex.Unlock()

	// --- 3. Execute CheckAndRemoveWhitelistedBlocks ---
	CheckAndRemoveWhitelistedBlocks()

	// Close listener and wait for goroutines
	listener.Close()
	wg.Wait()

	// --- 4. Verify in-memory state ---
	ActivityMutex.Lock()
	defer ActivityMutex.Unlock()

	// ip1Blocked (10.0.0.5) should be unblocked (because it's now in 10.0.0.0/8)
	if ActivityStore[TrackingKey{IP: ip1Blocked}].IsBlocked {
		t.Errorf("TC3 failed: Expected IP %s to be unblocked in memory, but IsBlocked is true", ip1Blocked)
	}
	// ip2WhitelistedBlocked (192.168.1.10) should be unblocked
	if ActivityStore[TrackingKey{IP: ip2WhitelistedBlocked}].IsBlocked {
		t.Errorf("TC3 failed: Expected IP %s to be unblocked in memory, but IsBlocked is true", ip2WhitelistedBlocked)
	}
	// ip3Blocked (1.1.1.1) should still be blocked
	if !ActivityStore[TrackingKey{IP: ip3Blocked}].IsBlocked {
		t.Errorf("TC3 failed: Expected IP %s to remain blocked in memory, but IsBlocked is false", ip3Blocked)
	}

	// --- 5. Verify HAProxy Commands ---
	expectedUnblockCommands := map[string]struct{}{
		// ip1 and ip2 should be unblocked from all tables
		"set table table_5m key 10.0.0.5 remove":     {},
		"set table table_1h key 10.0.0.5 remove":     {},
		"set table table_5m key 192.168.1.10 remove": {},
		"set table table_1h key 192.168.1.10 remove": {},
	}

	if len(commandsReceived) != 4 {
		t.Errorf("TC4 failed: Expected 4 HAProxy remove commands, got %d. Commands: %v", len(commandsReceived), commandsReceived)
	}
	for _, cmd := range commandsReceived {
		if _, ok := expectedUnblockCommands[cmd]; !ok {
			t.Errorf("TC4 failed: Received unexpected command: %s", cmd)
		}
	}
}

// =============================================================================
// V. Chain Logic Test (CheckChains)
// =============================================================================

func TestCheckChainsLogic(t *testing.T) {
	resetGlobalState()
	DryRun = true // Run in DryRun mode to use DryRunActivityStore

	// Mock Chains (mimicking a config load)
	ChainMutex.Lock()
	Chains = []BehavioralChain{
		{
			Name:          "TestChain",
			MatchKey:      "ip_ua",
			Action:        "block",
			BlockDuration: 5 * time.Minute,
			Steps: []StepDef{
				{
					Order: 1,
					CompiledRegexes: map[string]*regexp.Regexp{
						"Path": regexp.MustCompile("/step1"),
					},
					MaxDelayDuration: 5 * time.Second,
					MinDelayDuration: 1 * time.Second,
				},
				{
					Order: 2,
					CompiledRegexes: map[string]*regexp.Regexp{
						"Path":     regexp.MustCompile("/step2"),
						"Protocol": regexp.MustCompile("HTTP/2.0"),
					},
					MaxDelayDuration: 3 * time.Second,
					MinDelayDuration: 1 * time.Second,
				},
			},
		},
	}
	ChainMutex.Unlock()

	ip := "1.2.3.4"
	ua := "TestUA"
	key := TrackingKey{IP: ip, UA: ua}
	var activity *BotActivity
	var state StepState

	// --- 1. Initial Match (Step 1) ---
	t1 := time.Now()
	entry1 := &LogEntry{IP: ip, Path: "/step1", UserAgent: ua, Protocol: "HTTP/1.1", Timestamp: t1, Method: "GET", IPVersion: VersionIPv4}

	// FIX: Manually update LastRequestTime, as CheckChains is not responsible for this.
	DryRunActivityMutex.Lock()
	activity1 := GetOrCreateActivityUnsafe(DryRunActivityStore, key)
	activity1.LastRequestTime = t1
	DryRunActivityMutex.Unlock()

	CheckChains(entry1)

	DryRunActivityMutex.Lock()
	activity = DryRunActivityStore[key]
	state = activity.ChainProgress["TestChain"]
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t1) {
		t.Errorf("TC1: Expected step 1, time %v. Got step %d, time %v", t1, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 2. Min Delay Check (Failure, current step remains 1) ---
	t2 := t1.Add(500 * time.Millisecond) // < 1s min delay
	entry2Short := &LogEntry{IP: ip, Path: "/step2", UserAgent: ua, Protocol: "HTTP/2.0", Timestamp: t2, Method: "GET", IPVersion: VersionIPv4}

	// FIX: Manually update LastRequestTime
	DryRunActivityMutex.Lock()
	activity2 := GetOrCreateActivityUnsafe(DryRunActivityStore, key)
	activity2.LastRequestTime = t2
	DryRunActivityMutex.Unlock()

	CheckChains(entry2Short)

	DryRunActivityMutex.Lock()
	state = DryRunActivityStore[key].ChainProgress["TestChain"]
	if state.CurrentStep != 1 {
		t.Errorf("TC2: Expected step 1 (Min Delay check failed), got %d", state.CurrentStep)
	}
	DryRunActivityMutex.Unlock()

	// --- 3. Max Delay Check (Timeout, resets progress to 0, then matches step 1) ---
	t3 := t1.Add(6 * time.Second) // > 5s max delay (from step 1)
	entry3Reset := &LogEntry{IP: ip, Path: "/step1", UserAgent: ua, Protocol: "HTTP/1.1", Timestamp: t3, Method: "GET", IPVersion: VersionIPv4}

	// FIX: Manually update LastRequestTime
	DryRunActivityMutex.Lock()
	activity3 := GetOrCreateActivityUnsafe(DryRunActivityStore, key)
	activity3.LastRequestTime = t3
	DryRunActivityMutex.Unlock()

	CheckChains(entry3Reset) // Match step 1 again.

	DryRunActivityMutex.Lock()
	state = DryRunActivityStore[key].ChainProgress["TestChain"]
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t3) {
		t.Errorf("TC3: Expected reset then step 1, time %v. Got step %d, time %v", t3, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 4. Chain Completion (Max Delay is 3s for step 2) ---
	t4 := t3.Add(2 * time.Second) // 2s delay (> 1s min delay)
	entry4Complete := &LogEntry{IP: ip, Path: "/step2", UserAgent: ua, Protocol: "HTTP/2.0", Timestamp: t4, Method: "GET", IPVersion: VersionIPv4}

	// FIX: Manually update LastRequestTime
	DryRunActivityMutex.Lock()
	activity4 := GetOrCreateActivityUnsafe(DryRunActivityStore, key)
	activity4.LastRequestTime = t4
	DryRunActivityMutex.Unlock()

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

	// --- 5. Blocked Skip Check (Should not process this log line) ---
	t5 := t4.Add(1 * time.Minute) // Still within 5m block window

	// FIX: Simulate log line arrival by calling ProcessLogLine.
	// This function contains the logic to skip chain checks AND update activity.LastRequestTime.
	// logTimeFormat is retrieved from the uploaded log_parse.go file.
	const logTimeFormat = "02/Jan/2006:15:04:05 -0700"
	logLine5 := fmt.Sprintf("test.domain.com %s - - [%s] \"GET /anywhere HTTP/1.1\" 200 123 \"-\" \"%s\"", ip, t5.Format(logTimeFormat), ua)
	ProcessLogLine(logLine5, 5)

	DryRunActivityMutex.Lock()
	// Activity for IP+UA should not exist anymore because the chain reset (step 4), but the IP-only activity should exist and show blocked.
	ipActivity5 := DryRunActivityStore[ipOnlyKey]
	if !ipActivity5.IsBlocked {
		t.Fatalf("TC5 failed: IP %s should still be blocked in memory.", ip)
	}
	// Check the IP+UA key state again - it should not have been re-created or modified because the IP-only key block check should have skipped the chain checks.
	if _, exists := DryRunActivityStore[key]; exists {
		// Truncate t5 to the precision used by log_parse.go (which loses nanoseconds).
		expectedTime := t5.Truncate(time.Second)

		if !ipActivity5.LastRequestTime.Equal(expectedTime) {
			t.Fatalf("TC5 failed: Expected LastRequestTime for IP-only key to be %v (time of the skipped log). Got %v", expectedTime, ipActivity5.LastRequestTime)
		}
	}
	DryRunActivityMutex.Unlock()
}

// TestParseLogLine_MalformedGroupCount tests the case where the log line matches the regex,
// but the number of capturing groups is incorrect, triggering the "malformed essential fields" error.
func TestParseLogLine_MalformedGroupCount(t *testing.T) {
	// Save the original logRegex and restore it after the test.
	originalLogRegex := logRegex
	defer func() { logRegex = originalLogRegex }()

	// 1. Define a temporary regex that is structurally identical to the full one
	// (preventing the -1 panic) but intentionally broken to return match == nil.
	// Here, we remove the required space between VHost and IP to force the match failure.
	logRegex = regexp.MustCompile(
		`^(?P<VHost>\S+)(?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{3}) (?P<Size>\d+) \"(?P<Referrer>[^\\\"]*)\" \"(?P<UserAgent>[^\\\"]*)\"$`,
	)

	// 2. Define a perfectly valid log line (should fail the regex match).
	logLine := `musicbrainz.org 192.0.2.10 - - [28/Oct/2025:17:00:02 +0000] "GET /v4/start HTTP/1.1" 200 100 "-" "V4-Bot-A"`

	// 3. Call the parser.
	entry, err := ParseLogLine(logLine)

	// 4. Check results.
	if err == nil {
		t.Fatalf("Expected ParseLogLine to fail due to log line not matching regex, but it returned nil error and entry: %v", entry)
	}

	// The error should be about the line not matching the format.
	expectedErrMsg := "line does not match log format regex"

	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Expected error message to contain '%s', but got: %s", expectedErrMsg, err.Error())
	}
}
