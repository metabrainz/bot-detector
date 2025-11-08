package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadChainsFromYAML_Success(t *testing.T) {
	// --- Setup ---
	// Create a temporary valid YAML file
	yamlContent := `
version: "1.0"
whitelist_cidrs:
  - "192.168.1.0/24"
  - "2001:db8:abcd::/48" # IPv6 Network
  - "2001:db8::dead:beef" # Bare IPv6
  - "10.0.0.1" # Bare IP
haproxy_addresses:
  - "127.0.0.1:9999"
duration_tables:
  "5m": "table_5m"
  "1h": "table_1h"
default_block_duration: "1h"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "block"
    block_duration: "5m"
    steps:
      - max_delay: "10s" # Step 1
        field_matches:
          Path: "/login"
      - min_delay: "1s" # Step 2
        field_matches:
          Path: "/login/confirm"
  - name: "TestDefaultDurationChain"
    match_key: "ip"
    action: "block" # No block_duration, should use default
    steps:
      - field_matches: { Path: "/default" }
`
	// Use t.TempDir() to create a temporary directory that is automatically cleaned up.
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "chains.yaml")
	if err := os.WriteFile(tempFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write temp yaml file: %v", err)
	}

	// Point the global YAMLFilePath to our temp file
	originalPath := YAMLFilePath
	YAMLFilePath = tempFile
	t.Cleanup(func() {
		YAMLFilePath = originalPath
		resetGlobalState()
	})

	// --- Act ---
	loadedCfg, err := LoadChainsFromYAML() // Now returns *LoadedConfig, error

	// --- Assert ---
	if err != nil {
		t.Fatalf("LoadChainsFromYAML() returned an unexpected error: %v", err)
	}

	if len(loadedCfg.Chains) != 2 {
		t.Fatalf("Expected 2 chains to be loaded, got %d", len(loadedCfg.Chains))
	}

	if loadedCfg.Chains[0].Name != "TestChain" {
		t.Errorf("Expected chain name 'TestChain', got '%s'", loadedCfg.Chains[0].Name)
	}

	if loadedCfg.Chains[0].BlockDuration != 5*time.Minute {
		t.Errorf("Expected block duration 5m, got %v", loadedCfg.Chains[0].BlockDuration)
	}

	// Assert that the second chain received the default block duration
	if loadedCfg.Chains[1].BlockDuration != 1*time.Hour {
		t.Errorf("Expected default block duration of 1h for second chain, got %v", loadedCfg.Chains[1].BlockDuration)
	}

	// Assertions for the new two-step chain structure
	if len(loadedCfg.Chains[0].Steps) != 2 {
		t.Fatalf("Expected chain to have 2 steps, got %d", len(loadedCfg.Chains[0].Steps))
	}

	step1 := loadedCfg.Chains[0].Steps[0]
	if step1.Order != 1 {
		t.Errorf("Expected step 1 to have order 1, got %d", step1.Order)
	}

	step2 := loadedCfg.Chains[0].Steps[1]
	if step2.Order != 2 {
		t.Errorf("Expected step 2 to have order 2, got %d", step2.Order)
	}
	if step2.MinDelayDuration != 1*time.Second {
		t.Errorf("Expected step 2 to have min_delay of 1s, got %v", step2.MinDelayDuration)
	}

	if len(loadedCfg.WhitelistNets) != 4 {
		t.Errorf("Expected 4 whitelist CIDRs, got %d", len(loadedCfg.WhitelistNets))
	}

	if len(loadedCfg.HAProxyAddresses) != 1 {
		t.Errorf("Expected 1 HAProxy address, got %d", len(loadedCfg.HAProxyAddresses))
	}

	if len(loadedCfg.DurationToTableName) != 2 {
		t.Errorf("Expected 2 duration tables, got %d", len(loadedCfg.DurationToTableName))
	}
	if loadedCfg.BlockTableNameFallback != "table_1h" {
		t.Errorf("Expected fallback table 'table_1h', got '%s'", loadedCfg.BlockTableNameFallback)
	}

	if !IsIPWhitelistedInList(NewIPInfo("10.0.0.1"), loadedCfg.WhitelistNets) {
		t.Error("Expected bare IPv4 '10.0.0.1' to be whitelisted, but it was not.")
	}
}

func TestLoadChainsFromYAML_Errors(t *testing.T) {
	tests := []struct {
		name          string
		yamlContent   string
		expectedError string
	}{
		{
			name:          "File Not Found",
			yamlContent:   "", // No content, as the file won't be created
			expectedError: "failed to read YAML file",
		},
		{
			name: "Unsupported Version",
			yamlContent: `
version: "0.9"
chains: []
`,
			expectedError: "configuration version mismatch",
		},
		{
			name: "Unknown Field (Strict Parsing)",
			yamlContent: `
version: "1.0"
unknown_field: "some_value"
chains: []
`,
			expectedError: "unknown field",
		},
		{
			name: "Invalid CIDR",
			yamlContent: `
version: "1.0"
whitelist_cidrs: ["192.168.1.0/33"]
chains: []
`,
			expectedError: "invalid CIDR",
		},
		{
			name: "Invalid Non-CIDR in Whitelist",
			yamlContent: `
version: "1.0"
whitelist_cidrs: ["not-an-ip"]
chains: []
`,
			expectedError: "invalid CIDR",
		},
		{
			name: "Invalid Duration",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    action: "block"
    block_duration: "5p"
`,
			expectedError: "invalid block_duration",
		},
		{
			name: "Invalid Duration in DurationTables",
			yamlContent: `
version: "1.0"
duration_tables:
  "1x": "table_1x"
`,
			expectedError: "invalid duration",
		},
		{
			name: "Missing Match Key",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    action: "log"
`,
			expectedError: "match_key cannot be empty",
		},
		{
			name: "Invalid Max Delay",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { max_delay: "10x" } ]
`,
			expectedError: "invalid max_delay",
		},
		{
			name: "Invalid Min Delay",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { min_delay: "10x" } ]
`,
			expectedError: "invalid min_delay",
		},
		{
			name: "Invalid Min Time Since Last Hit",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { min_time_since_last_hit: "10x" } ]
`,
			expectedError: "invalid min_time_since_last_hit",
		},
		{
			name: "Invalid Regex",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "Path": "/(" } } ]
`,
			expectedError: "failed to compile regex",
		},
		{
			name: "Invalid Default Block Duration",
			yamlContent: `
version: "1.0"
default_block_duration: "1x"
chains: []
`,
			expectedError: "invalid block_duration format",
		},
		{
			name: "Block Action Missing Duration",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    action: "block" # No block_duration and no default
`,
			expectedError: "block_duration is missing or zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			tempFile := filepath.Join(tempDir, "test_chains.yaml")
			if tt.yamlContent != "" {
				os.WriteFile(tempFile, []byte(tt.yamlContent), 0644)
			}

			originalPath := YAMLFilePath
			YAMLFilePath = tempFile
			t.Cleanup(func() { YAMLFilePath = originalPath })

			_, err := LoadChainsFromYAML()

			if err == nil || !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing '%s', but got: %v", tt.expectedError, err)
			}
		})
	}
}

func TestCheckAndRemoveWhitelistedBlocks(t *testing.T) {
	tests := []struct {
		name            string
		blockedIP       string
		expectedCommand string
	}{
		{"IPv4", "192.0.2.100", "clear table table_5m_ipv4 key 192.0.2.100"},
		{"IPv6", "2001:db8::dead:beef", "clear table table_5m_ipv6 key 2001:db8::dead:beef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup for each sub-test ---
			resetGlobalState()
			t.Cleanup(resetGlobalState)

			// Create a processor instance for the test.
			processor := &Processor{
				ActivityStore: make(map[TrackingKey]*BotActivity),
				ActivityMutex: &sync.RWMutex{},
				ChainMutex:    &sync.RWMutex{},
				Config: &AppConfig{
					// This config is needed for the p.UnblockIP call to work.
					HAProxyAddresses:    []string{"127.0.0.1:9999"},
					DurationToTableName: map[time.Duration]string{5 * time.Minute: "table_5m"},
				},
				// Capture log output for assertion.
				LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {
					// For this test, we only care about the WHITELIST_UNBLOCK log.
				},
			}

			var commandsReceived []string
			mockExecutor := func(addr, ip, command string) error {
				commandsReceived = append(commandsReceived, strings.TrimSpace(command))
				return nil
			}
			setupMockExecutor(t, mockExecutor)

			// 2. Define the IP that is currently blocked but will be whitelisted.
			trackingKey := TrackingKey{IPInfo: NewIPInfo(tt.blockedIP)}
			blockExpirationTime := time.Now().Add(time.Hour)

			// 3. Manually set the state in ActivityStore to simulate a blocked IP.
			processor.ActivityStore[trackingKey] = &BotActivity{
				IsBlocked:    true,
				BlockedUntil: blockExpirationTime,
			}

			// Capture the specific log message we care about.
			var capturedLog string
			processor.LogFunc = func(level LogLevel, tag string, format string, args ...interface{}) {
				if tag == "WHITELIST_UNBLOCK" {
					capturedLog = fmt.Sprintf(format, args...)
				}
			}

			// 4. Set the WhitelistNets on the processor's config to include the blocked IP.
			_, ipNet, _ := net.ParseCIDR(tt.blockedIP + "/32")
			processor.Config.WhitelistNets = []*net.IPNet{ipNet}

			// --- Act ---
			processor.CheckAndRemoveWhitelistedBlocks()

			// --- Assert ---
			// Assert Log Output
			expectedLogSubstring := blockExpirationTime.Format(AppLogTimestampFormat)
			if !strings.Contains(capturedLog, expectedLogSubstring) {
				t.Errorf("Expected log message to contain the original block time '%s', but it did not. Log was: '%s'",
					expectedLogSubstring, capturedLog)
			}

			// Assert HAProxy Command
			if len(commandsReceived) != 1 || commandsReceived[0] != tt.expectedCommand {
				t.Errorf("Expected unblock command '%s', but got %v", tt.expectedCommand, commandsReceived)
			}

			// Assert Final State
			activity, exists := processor.ActivityStore[trackingKey]
			if !exists {
				t.Fatal("Activity for the IP was unexpectedly deleted.")
			}
			if activity.IsBlocked {
				t.Error("Expected IsBlocked to be false after whitelist cleanup, but it was true.")
			}
		})
	}

	t.Run("Unblock Fails", func(t *testing.T) {
		// --- Setup ---
		resetGlobalState()
		t.Cleanup(resetGlobalState)

		// Mock the UnblockIP function to return an error.
		// We need to do this by mocking the underlying executor.
		mockExecutor := func(addr, ip, command string) error {
			return fmt.Errorf("simulated HAProxy failure")
		}
		setupMockExecutor(t, mockExecutor)

		processor := &Processor{
			ActivityStore: make(map[TrackingKey]*BotActivity),
			ActivityMutex: &sync.RWMutex{},
			ChainMutex:    &sync.RWMutex{},
			LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {},
			Config: &AppConfig{
				HAProxyAddresses:    []string{"127.0.0.1:9999"},
				DurationToTableName: map[time.Duration]string{time.Minute: "t1"},
			},
		}

		// Manually set a blocked IP that is also on the whitelist.
		blockedIP := "192.0.2.100"
		trackingKey := TrackingKey{IPInfo: NewIPInfo(blockedIP)}
		processor.ActivityStore[trackingKey] = &BotActivity{
			IsBlocked:    true,
			BlockedUntil: time.Now().Add(time.Hour),
		}
		_, ipNet, _ := net.ParseCIDR(blockedIP + "/32")
		processor.Config.WhitelistNets = []*net.IPNet{ipNet}

		// --- Act ---
		processor.CheckAndRemoveWhitelistedBlocks()

		// --- Assert ---
		// The IP should remain blocked in memory because the HAProxy command failed.
		if !processor.ActivityStore[trackingKey].IsBlocked {
			t.Error("Expected IsBlocked to remain true after a failed unblock attempt, but it was set to false.")
		}
	})
}

func TestChainWatcher_Reload(t *testing.T) {
	// --- Setup ---
	// 1. Create a temporary YAML file with initial content.
	initialYAMLContent := `
version: "1.0"
log_level: "info"
whitelist_cidrs: ["1.1.1.1/32"]
chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/initial"}}]
`
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "chains.yaml")
	if err := os.WriteFile(tempFile, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}

	// Point the global YAMLFilePath to our temp file for the duration of the test.
	originalPath := YAMLFilePath
	YAMLFilePath = tempFile
	t.Cleanup(func() { YAMLFilePath = originalPath })

	// 2. Load the initial configuration.
	initialLoadedCfg, err := LoadChainsFromYAML()
	if err != nil {
		t.Fatalf("Initial LoadChainsFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config.
	processor := &Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        initialLoadedCfg.Chains,
		ChainMutex:    &sync.RWMutex{},
		LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {},
		Config: &AppConfig{
			testOverridePollingInterval: 10 * time.Millisecond, // Use a very short poll interval for the test.
		},
	}
	// Set LastModTime to the actual modification time of the initial file.
	initialFileInfo, err := os.Stat(tempFile)
	if err != nil {
		t.Fatalf("Failed to stat initial temp yaml file: %v", err)
	}
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 4. Start the ChainWatcher in a goroutine.
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) }) // Ensure watcher stops when test finishes.

	go processor.ChainWatcher(stopWatcher)

	// Give the watcher a moment to start and potentially read the initial file.
	time.Sleep(processor.Config.testOverridePollingInterval * 2)

	// --- Act ---
	// 5. Modify the YAML file on disk.
	time.Sleep(100 * time.Millisecond) // Wait a moment to ensure the modification time is different.
	modifiedYAMLContent := `
version: "1.0"
log_level: "debug" # Changed log level
whitelist_cidrs: ["1.1.1.1/32", "2.2.2.2/32"] # Added a new CIDR (1.1.1.1/32 was already there)
chains:
  - name: "ReloadedChain" # Changed chain name
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/reloaded"}}]
`
	if err := os.WriteFile(tempFile, []byte(modifiedYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write modified temp yaml file: %v", err)
	}

	// 6. Wait for the watcher to detect and apply the changes.
	time.Sleep(processor.Config.testOverridePollingInterval * 2) // Wait for at least two polling intervals

	// --- Assert ---
	// 7. Check if the processor's state has been updated.
	processor.ChainMutex.RLock()
	defer processor.ChainMutex.RUnlock()

	if len(processor.Chains) != 1 || processor.Chains[0].Name != "ReloadedChain" {
		t.Errorf("Expected chain to be 'ReloadedChain', but got: %+v", processor.Chains)
	}
	if len(processor.Config.WhitelistNets) != 2 {
		t.Errorf("Expected 2 whitelist networks, but got %d", len(processor.Config.WhitelistNets))
	}
	if CurrentLogLevel != LevelDebug {
		t.Errorf("Expected log level to be updated to 'debug', but it was not.")
	}
}

func TestChainWatcher_ReloadFailure(t *testing.T) {
	// --- Setup ---
	// 1. Create a temporary YAML file with initial valid content.
	initialYAMLContent := `
version: "1.0"
chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/initial"}}]
`
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "chains.yaml")
	if err := os.WriteFile(tempFile, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}

	originalPath := YAMLFilePath
	YAMLFilePath = tempFile
	t.Cleanup(func() { YAMLFilePath = originalPath })

	// 2. Load the initial configuration.
	initialLoadedCfg, err := LoadChainsFromYAML()
	if err != nil {
		t.Fatalf("Initial LoadChainsFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config and a log capturer.
	var capturedLogs []string
	var logMutex sync.Mutex
	processor := &Processor{
		Chains:     initialLoadedCfg.Chains,
		ChainMutex: &sync.RWMutex{},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {
			logMutex.Lock()
			capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
			logMutex.Unlock()
		},
		Config: &AppConfig{testOverridePollingInterval: 10 * time.Millisecond},
	}
	initialFileInfo, _ := os.Stat(tempFile)
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 4. Start the ChainWatcher.
	stopWatcher := make(chan struct{})
	go processor.ChainWatcher(stopWatcher)
	t.Cleanup(func() { close(stopWatcher) })

	// --- Act ---
	// 5. Modify the YAML file with INVALID content.
	time.Sleep(100 * time.Millisecond)
	invalidYAMLContent := `version: "1.0"\nchains: [ { name: "Invalid", steps: [ { field_matches: { "Path": "*invalid-regex" } } ] } ]`
	os.WriteFile(tempFile, []byte(invalidYAMLContent), 0644)

	// 6. Wait for the watcher to attempt the reload.
	time.Sleep(processor.Config.testOverridePollingInterval * 2)

	// --- Assert ---
	// 7. Check that an error was logged and the original config is still active.
	logOutput := strings.Join(capturedLogs, "\n")
	if !strings.Contains(logOutput, "LOAD_ERROR: Failed to reload chains") {
		t.Errorf("Expected a 'LOAD_ERROR' log message, but none was found. Logs:\n%s", logOutput)
	}
	if len(processor.Chains) != 1 || processor.Chains[0].Name != "InitialChain" {
		t.Errorf("Processor chains were modified despite reload failure. Expected 'InitialChain', got: %+v", processor.Chains)
	}
}

func TestChainWatcher_StatError(t *testing.T) {
	// --- Setup ---
	// 1. Create a temporary YAML file.
	initialYAMLContent := `
version: "1.0"
chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/initial"}}]
`
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "chains.yaml")
	if err := os.WriteFile(tempFile, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}

	originalPath := YAMLFilePath
	YAMLFilePath = tempFile
	t.Cleanup(func() { YAMLFilePath = originalPath })

	// 2. Create the processor with a log capturer.
	var capturedLogs []string
	var logMutex sync.Mutex
	processor := &Processor{
		Chains:     []BehavioralChain{{Name: "InitialChain"}}, // Simplified initial state
		ChainMutex: &sync.RWMutex{},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {
			logMutex.Lock()
			capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
			logMutex.Unlock()
		},
		Config: &AppConfig{testOverridePollingInterval: 10 * time.Millisecond},
	}
	initialFileInfo, _ := os.Stat(tempFile)
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 3. Start the ChainWatcher.
	stopWatcher := make(chan struct{})
	go processor.ChainWatcher(stopWatcher)
	t.Cleanup(func() { close(stopWatcher) })

	// --- Act ---
	// 4. Delete the YAML file to trigger a stat error on the next poll.
	time.Sleep(50 * time.Millisecond) // Ensure at least one successful poll happens first.
	if err := os.Remove(tempFile); err != nil {
		t.Fatalf("Failed to remove temp file: %v", err)
	}

	// 5. Wait for the watcher to attempt the reload and fail.
	time.Sleep(processor.Config.testOverridePollingInterval * 2)

	// --- Assert ---
	// 6. Check that an error was logged.
	logOutput := strings.Join(capturedLogs, "\n")
	if !strings.Contains(logOutput, "WATCH_ERROR: Failed to stat file") {
		t.Errorf("Expected a 'WATCH_ERROR' log message, but none was found. Logs:\n%s", logOutput)
	}
}
