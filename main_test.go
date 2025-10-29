package main

import (
	"bufio"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
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

	// Reset config-related globals
	ChainMutex.Lock()
	Chains = nil
	WhitelistNets = nil
	LastModTime = time.Time{}
	ChainMutex.Unlock()

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
		t.Errorf("Test Case 1 failed: Expected log level to be %v, got %v", LevelInfo, CurrentLogLevel)
	}

	// Test Case 2: Valid durations and log level (Production Mode)
	resetGlobalState()
	DryRun = false
	LogLevelStr = "debug"
	PollingIntervalStr = "10s"
	CleanupIntervalStr = "5m"
	IdleTimeoutStr = "1h"

	if err := ParseDurations(); err != nil {
		t.Fatalf("Test Case 2 failed: Expected no error, got %v", err)
	}
	if PollingInterval != 10*time.Second || CleanupInterval != 5*time.Minute || IdleTimeout != 1*time.Hour {
		t.Errorf("Test Case 2 failed: Duration mismatch. Got: %v, %v, %v", PollingInterval, CleanupInterval, IdleTimeout)
	}
	if CurrentLogLevel != LevelDebug {
		t.Errorf("Test Case 2 failed: Expected log level to be %v, got %v", LevelDebug, CurrentLogLevel)
	}

	// Test Case 3: Invalid log level
	resetGlobalState()
	LogLevelStr = "invalid"
	if err := ParseDurations(); err == nil || !strings.Contains(err.Error(), "invalid log-level") {
		t.Errorf("Test Case 3 failed: Expected 'invalid log-level' error, got %v", err)
	}

	// Test Case 4: Invalid polling interval format
	resetGlobalState()
	DryRun = false
	LogLevelStr = "warning"
	PollingIntervalStr = "10sec"
	if err := ParseDurations(); err == nil || !strings.Contains(err.Error(), "invalid poll-interval") {
		t.Errorf("Test Case 4 failed: Expected 'invalid poll-interval' error, got %v", err)
	}
}

// =============================================================================
// II. Log and IP Helpers Tests
// =============================================================================

func TestGetIPVersion(t *testing.T) {
	tests := []struct {
		ip       string
		expected string
	}{
		{"192.168.1.1", VersionIPv4},
		{"2001:0db8:85a3:0000:0000:8a2e:0370:7334", VersionIPv6},
		{"1.2.3.4.5", VersionInvalid},
		{"not-an-ip", VersionInvalid},
		{"", VersionInvalid},
	}

	for _, test := range tests {
		actual := GetIPVersion(test.ip)
		if actual != test.expected {
			t.Errorf("IP: %s, Expected: %s, Got: %s", test.ip, test.expected, actual)
		}
	}
}

func TestParseLogLine(t *testing.T) {
	// Sample HAProxy/Apache Combined Log Format (simplified for core fields)
	logLine := `127.0.0.1 - - [29/Oct/2025:10:00:00 +0100] "GET /api/v1/user/123 HTTP/1.1" 200 456 "-" "Mozilla/5.0 (Bot)"`
	entry, err := ParseLogLine(logLine)

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if entry.IP != "127.0.0.1" {
		t.Errorf("Expected IP 127.0.0.1, got %s", entry.IP)
	}
	if entry.Method != "GET" {
		t.Errorf("Expected Method GET, got %s", entry.Method)
	}
	if entry.Path != "/api/v1/user/123" {
		t.Errorf("Expected Path /api/v1/user/123, got %s", entry.Path)
	}
	if entry.StatusCode != 200 {
		t.Errorf("Expected Status Code 200, got %d", entry.StatusCode)
	}
	if entry.UserAgent != "Mozilla/5.0 (Bot)" {
		t.Errorf("Expected User Agent 'Mozilla/5.0 (Bot)', got %s", entry.UserAgent)
	}
	expectedTime := time.Date(2025, time.October, 29, 10, 0, 0, 0, time.FixedZone("", 3600)) // +0100 is 3600 seconds
	if !entry.Timestamp.Equal(expectedTime) {
		t.Errorf("Expected Time %v, got %v", expectedTime, entry.Timestamp)
	}

	// Test Case: Malformed line (too few quotes)
	malformedLine := `127.0.0.1 - - [29/Oct/2025:10:00:00 +0100] "GET /path HTTP/1.1" 200`
	_, err = ParseLogLine(malformedLine)
	if err == nil {
		t.Error("Expected error for malformed line, got nil")
	}
}

func TestGetTrackingKey(t *testing.T) {
	entry := &LogEntry{IP: "192.168.1.1", UserAgent: "TestAgent/1.0"}
	ipv6Entry := &LogEntry{IP: "::1", UserAgent: "TestAgent/1.0"}

	tests := []struct {
		name     string
		chainKey string
		entry    *LogEntry
		expected TrackingKey
	}{
		{"IP only (IPv4)", "ip", entry, TrackingKey{IP: "192.168.1.1", UA: ""}},
		{"IP only (IPv6)", "ip", ipv6Entry, TrackingKey{IP: "::1", UA: ""}},
		{"IP+UA (IPv4)", "ip_ua", entry, TrackingKey{IP: "192.168.1.1", UA: "TestAgent/1.0"}},
		{"IPv4 only (Match)", "ipv4", entry, TrackingKey{IP: "192.168.1.1", UA: ""}},
		{"IPv4 only (Mismatch)", "ipv4", ipv6Entry, TrackingKey{}}, // Expected empty key
		{"IPv6 only (Match)", "ipv6", ipv6Entry, TrackingKey{IP: "::1", UA: ""}},
		{"IPv6 only (Mismatch)", "ipv6", entry, TrackingKey{}}, // Expected empty key
	}

	for _, test := range tests {
		chain := BehavioralChain{MatchKey: test.chainKey}
		actual := GetTrackingKey(&chain, test.entry)
		if actual != test.expected {
			t.Errorf("Test %s failed (Key: %s): Expected %v, got %v", test.name, test.chainKey, test.expected, actual)
		}
	}
}

// =============================================================================
// III. Whitelist and Blocking Tests
// =============================================================================

func TestIsIPWhitelisted(t *testing.T) {
	resetGlobalState()

	// Setup: Populate WhitelistNets
	_, net1, _ := net.ParseCIDR("10.0.0.0/24")
	_, net2, _ := net.ParseCIDR("2001:db8::/32")

	ChainMutex.Lock()
	WhitelistNets = []*net.IPNet{net1, net2}
	ChainMutex.Unlock()

	tests := []struct {
		ip       string
		expected bool
	}{
		{"10.0.0.1", true},         // Inside CIDR 1
		{"10.0.1.1", false},        // Outside CIDR 1
		{"2001:db8:1234::1", true}, // Inside CIDR 2
		{"2001:db9::1", false},     // Outside CIDR 2
		{"192.168.1.1", false},     // Standard public IP
		{"invalid-ip", false},      // Invalid IP
	}

	for _, test := range tests {
		actual := IsIPWhitelisted(test.ip)
		if actual != test.expected {
			t.Errorf("IP: %s, Expected Whitelisted: %v, Got: %v", test.ip, test.expected, actual)
		}
	}
}

// Test BlockIPForDuration in Dry Run mode only, as actual socket connection is hard to mock.
func TestBlockIPForDurationDryRun(t *testing.T) {
	resetGlobalState()
	DryRun = true
	// We're just testing that it doesn't return an error and logs correctly in dry-run mode.
	err := BlockIPForDuration("1.2.3.4", 5*time.Minute)
	if err != nil {
		t.Errorf("Dry-run BlockIPForDuration failed: expected nil error, got %v", err)
	}
}

// =============================================================================
// IV. YAML Loading Tests
// =============================================================================

func TestLoadChainsFromYAML(t *testing.T) {
	resetGlobalState()

	// 1. Setup a temporary valid YAML file
	validYAML := `
version: "1.0"
whitelist_cidrs:
  - "127.0.0.1/32"
  - "10.0.0.0/8"
chains:
  - name: "ScannerChain"
    match_key: "ipv4"
    action: "block"
    block_duration: "30m"
    steps:
      - order: 1
        field_matches:
          Path: "/wp-login.php"
        max_delay: "10s"
      - order: 2
        field_matches:
          StatusCode: "^404$"
        min_delay: "1s"
`
	// Use a temporary file path
	YAMLFilePath = "test_config_valid.yaml"
	if err := os.WriteFile(YAMLFilePath, []byte(validYAML), 0644); err != nil {
		t.Fatalf("Failed to write temporary YAML file: %v", err)
	}
	defer os.Remove(YAMLFilePath)

	// 2. Test valid configuration load
	chains, err := LoadChainsFromYAML()
	if err != nil {
		t.Fatalf("Failed to load valid YAML: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("Expected 1 chain, got %d", len(chains))
	}

	// Verify Chain Properties
	chain := chains[0]
	if chain.Name != "ScannerChain" || chain.MatchKey != "ipv4" || chain.Action != "block" || chain.BlockDuration != 30*time.Minute {
		t.Errorf("Chain properties incorrect: %+v", chain)
	}

	// Verify Step 1 Properties
	step1 := chain.Steps[0]
	if step1.Order != 1 || step1.MaxDelayDuration != 10*time.Second || step1.MinDelayDuration != 0 {
		t.Errorf("Step 1 properties incorrect: %+v", step1)
	}
	if _, ok := step1.CompiledRegexes["Path"]; !ok || step1.CompiledRegexes["Path"].String() != "/wp-login.php" {
		t.Errorf("Step 1 regex missing or incorrect")
	}

	// Verify Step 2 Properties
	step2 := chain.Steps[1]
	if step2.Order != 2 || step2.MaxDelayDuration != 0 || step2.MinDelayDuration != 1*time.Second {
		t.Errorf("Step 2 properties incorrect: %+v", step2)
	}
	if _, ok := step2.CompiledRegexes["StatusCode"]; !ok || step2.CompiledRegexes["StatusCode"].String() != "^404$" {
		t.Errorf("Step 2 regex missing or incorrect")
	}

	// Verify Whitelist Nets
	ChainMutex.RLock()
	if len(WhitelistNets) != 2 {
		t.Errorf("Expected 2 whitelist nets, got %d", len(WhitelistNets))
	}
	ChainMutex.RUnlock()

	// 3. Test Invalid Regex
	invalidRegexYAML := `
version: "1.0"
chains:
  - name: "InvalidChain"
    action: "log"
    steps:
      - order: 1
        field_matches:
          Path: "[invalid-regex" 
`
	YAMLFilePath = "test_config_invalid_regex.yaml"
	os.WriteFile(YAMLFilePath, []byte(invalidRegexYAML), 0644)
	defer os.Remove(YAMLFilePath)

	_, err = LoadChainsFromYAML()
	if err == nil || !strings.Contains(err.Error(), "failed to compile regex") {
		t.Errorf("Expected 'failed to compile regex' error, got %v", err)
	}

	// 4. Test Invalid Block Duration
	invalidDurationYAML := `
version: "1.0"
chains:
  - name: "InvalidDuration"
    action: "block"
    block_duration: "30min"
    steps:
      - order: 1
        field_matches:
          Path: "/test"
`
	YAMLFilePath = "test_config_invalid_duration.yaml"
	os.WriteFile(YAMLFilePath, []byte(invalidDurationYAML), 0644)
	defer os.Remove(YAMLFilePath)

	_, err = LoadChainsFromYAML()
	if err == nil || !strings.Contains(err.Error(), "failed to parse block_duration") {
		t.Errorf("Expected 'failed to parse block_duration' error, got %v", err)
	}
}

// =============================================================================
// V. Core Logic Helpers Tests
// =============================================================================

func TestGetMatchValue(t *testing.T) {
	entry := &LogEntry{
		IP:         "1.1.1.1",
		Path:       "/index.html",
		Method:     "GET",
		UserAgent:  "Test",
		Referrer:   "http://example.com",
		StatusCode: 200,
	}

	tests := map[string]string{
		"IP":         "1.1.1.1",
		"Path":       "/index.html",
		"Method":     "GET",
		"UserAgent":  "Test",
		"Referrer":   "http://example.com",
		"StatusCode": "200",
	}

	for field, expected := range tests {
		actual, err := GetMatchValue(field, entry)
		if err != nil {
			t.Fatalf("Field %s: Unexpected error: %v", field, err)
		}
		if actual != expected {
			t.Errorf("Field %s: Expected %s, got %s", field, expected, actual)
		}
	}

	// Test unknown field
	_, err := GetMatchValue("UnknownField", entry)
	if err == nil {
		t.Error("Expected error for unknown field, got nil")
	}
}

func TestGetOrCreateActivityUnsafe(t *testing.T) {
	resetGlobalState()
	// Since GetOrCreateActivityUnsafe is non-locking, we must acquire the mutex here.
	ActivityMutex.Lock()
	defer ActivityMutex.Unlock()

	key1 := TrackingKey{IP: "192.168.1.1", UA: "A"}

	// Test 1: Creation
	activity1 := GetOrCreateActivityUnsafe(ActivityStore, key1)
	if len(ActivityStore) != 1 {
		t.Fatalf("Expected 1 entry after creation, got %d", len(ActivityStore))
	}
	if activity1.ChainProgress == nil {
		t.Error("ChainProgress should be initialized")
	}

	// Test 2: Retrieval (should return the same object)
	activity2 := GetOrCreateActivityUnsafe(ActivityStore, key1)
	if activity1 != activity2 {
		t.Error("Expected to retrieve the same activity object")
	}
	if len(ActivityStore) != 1 {
		t.Fatalf("Expected 1 entry after retrieval, got %d", len(ActivityStore))
	}

	// Test 3: Creation of a new key
	key2 := TrackingKey{IP: "192.168.1.2", UA: "B"}
	activity3 := GetOrCreateActivityUnsafe(ActivityStore, key2)
	if activity3 == activity1 {
		t.Error("Activity for key2 should be a new object")
	}
	if len(ActivityStore) != 2 {
		t.Fatalf("Expected 2 entries after second creation, got %d", len(ActivityStore))
	}
}

// =============================================================================
// VI. ReadLineWithLimit Tests
// =============================================================================

func TestReadLineWithLimit(t *testing.T) {
	// Test Case 1: Simple line read
	reader1 := strings.NewReader("line 1\nline 2")
	bufReader1 := bufio.NewReader(reader1)
	line1, err1 := ReadLineWithLimit(bufReader1, 100)

	if err1 != nil {
		t.Fatalf("TC1: Unexpected error: %v", err1)
	}
	if line1 != "line 1" {
		t.Errorf("TC1: Expected 'line 1', got '%s'", line1)
	}

	// Test Case 2: Line exactly at limit (should succeed)
	lineAtLimit := strings.Repeat("a", 5) + "\n"
	reader2 := strings.NewReader(lineAtLimit)
	bufReader2 := bufio.NewReader(reader2)
	line2, err2 := ReadLineWithLimit(bufReader2, 5)

	if err2 != nil {
		t.Fatalf("TC2: Unexpected error: %v", err2)
	}
	if line2 != strings.Repeat("a", 5) {
		t.Errorf("TC2: Line incorrect. Got: '%s'", line2)
	}

	// Test Case 3: Line exceeding small limit (should fail and drain)
	lineOverLimit := strings.Repeat("b", 6) + "cde\nnext line\n"
	reader3 := strings.NewReader(lineOverLimit)
	bufReader3 := bufio.NewReader(reader3)
	line3, err3 := ReadLineWithLimit(bufReader3, 5)

	if !errors.Is(err3, ErrLineSkipped) {
		t.Fatalf("TC3: Expected ErrLineSkipped, got %v", err3)
	}
	if line3 != "" {
		t.Errorf("TC3: Expected empty line, got '%s'", line3)
	}

	// Verify that the rest of the line was drained (next read should be "next line")
	line3Next, err3Next := ReadLineWithLimit(bufReader3, 100)
	if err3Next != nil {
		t.Fatalf("TC3 Next: Unexpected error: %v", err3Next)
	}
	if line3Next != "next line" {
		t.Errorf("TC3 Next: Expected 'next line', got '%s'", line3Next)
	}

	// Test Case 4: Line exceeding critical limit (MaxLogLineSize, 16KB)
	// We create a line 1 byte longer than the critical limit.
	lineOverCriticalLimit := strings.Repeat("x", MaxLogLineSize+1) + "\n"
	reader4 := strings.NewReader(lineOverCriticalLimit)
	bufReader4 := bufio.NewReader(reader4)
	line4, err4 := ReadLineWithLimit(bufReader4, MaxLogLineSize)

	if !errors.Is(err4, ErrLineSkipped) {
		t.Fatalf("TC4: Expected ErrLineSkipped for >16KB line, got %v", err4)
	}
	if line4 != "" {
		t.Errorf("TC4: Expected empty line, got '%s'", line4)
	}

	// Add a next line check to ensure the reader is usable after the massive skip
	line4Next := "small line after huge skip"
	reader4.Reset(lineOverCriticalLimit + line4Next + "\n")
	bufReader4.Reset(reader4)
	// Skip the long line
	ReadLineWithLimit(bufReader4, MaxLogLineSize)

	line4NextRead, err4Next := ReadLineWithLimit(bufReader4, 100)
	if err4Next != nil {
		t.Fatalf("TC4 Next: Unexpected error after critical skip: %v", err4Next)
	}
	if line4NextRead != line4Next {
		t.Errorf("TC4 Next: Expected '%s', got '%s'", line4Next, line4NextRead)
	}
}

// =============================================================================
// VII. Integration-like Test (CheckChains logic)
// =============================================================================

func TestCheckChains(t *testing.T) {
	resetGlobalState()
	DryRun = true // Use dry run mode to prevent real HAProxy calls and use DryRunActivityStore

	// Setup: Define a two-step chain
	// Step 1: GET /step1, MaxDelay 5s
	// Step 2: GET /step2, MinDelay 1s
	step1 := StepDef{
		Order:            1,
		FieldMatches:     map[string]string{"Path": "/step1"},
		MaxDelayDuration: 5 * time.Second,
		CompiledRegexes:  map[string]*regexp.Regexp{"Path": regexp.MustCompile("/step1")},
	}
	step2 := StepDef{
		Order:            2,
		FieldMatches:     map[string]string{"Path": "/step2"},
		MinDelayDuration: 1 * time.Second,
		CompiledRegexes:  map[string]*regexp.Regexp{"Path": regexp.MustCompile("/step2")},
	}
	testChain := BehavioralChain{
		Name:          "TestChain",
		Steps:         []StepDef{step1, step2},
		Action:        "block",
		BlockDuration: 10 * time.Minute,
		MatchKey:      "ip",
	}

	ChainMutex.Lock()
	Chains = []BehavioralChain{testChain}
	ChainMutex.Unlock()

	// Initial time
	t1 := time.Date(2025, time.January, 1, 10, 0, 0, 0, time.UTC)
	ip := "1.2.3.4"
	key := TrackingKey{IP: ip, UA: ""}

	// --- 1. Step 1 Match ---
	entry1 := &LogEntry{IP: ip, Path: "/step1", Timestamp: t1, Method: "GET"}
	CheckChains(entry1)

	DryRunActivityMutex.Lock()
	activity := DryRunActivityStore[key]

	if activity == nil {
		t.Fatal("TC1: Activity not created.")
	}
	state := activity.ChainProgress["TestChain"]
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t1) {
		t.Errorf("TC1: Expected step 1, time %v. Got step %d, time %v", t1, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 2. Min Delay Check (Too short delay) ---
	t2 := t1.Add(500 * time.Millisecond) // 0.5s delay
	entry2Short := &LogEntry{IP: ip, Path: "/step2", Timestamp: t2, Method: "GET"}
	CheckChains(entry2Short)

	DryRunActivityMutex.Lock()
	state = DryRunActivityStore[key].ChainProgress["TestChain"]
	if state.CurrentStep != 1 {
		t.Errorf("TC2: Expected step 1 (Min Delay check failed), got %d", state.CurrentStep)
	}
	DryRunActivityMutex.Unlock()

	// --- 3. Max Delay Check (Timeout, resets progress to 0) ---
	t3 := t1.Add(6 * time.Second) // > 5s max delay (from step 1)
	entry3Reset := &LogEntry{IP: ip, Path: "/step1", Timestamp: t3, Method: "GET"}
	CheckChains(entry3Reset) // Match step 1 again.

	DryRunActivityMutex.Lock()
	state = DryRunActivityStore[key].ChainProgress["TestChain"]
	if state.CurrentStep != 1 || !state.LastMatchTime.Equal(t3) {
		t.Errorf("TC3: Expected reset then step 1, time %v. Got step %d, time %v", t3, state.CurrentStep, state.LastMatchTime)
	}
	DryRunActivityMutex.Unlock()

	// --- 4. Chain Completion ---
	t4 := t3.Add(2 * time.Second) // 2s delay (> 1s min delay)
	entry4Complete := &LogEntry{IP: ip, Path: "/step2", Timestamp: t4, Method: "GET"}
	CheckChains(entry4Complete)

	DryRunActivityMutex.Lock()
	activity = DryRunActivityStore[key]
	// Activity should be marked as blocked
	if !activity.IsBlocked {
		t.Errorf("TC4: Expected activity to be blocked.")
	}
	// Progress should be reset and removed from map
	if _, exists := activity.ChainProgress["TestChain"]; exists {
		t.Errorf("TC4: ChainProgress should have been reset and removed from map after completion.")
	}
	DryRunActivityMutex.Unlock()
}
