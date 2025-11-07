package main

import (
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
			tempFile := filepath.Join(tempDir, "chains.yaml")
			os.WriteFile(tempFile, []byte(tt.yamlContent), 0644)

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
				LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
			}

			var commandsReceived []string
			mockExecutor := func(addr, ip, command string) error {
				commandsReceived = append(commandsReceived, strings.TrimSpace(command))
				return nil
			}
			setupMockExecutor(t, mockExecutor)

			// 2. Define the IP that is currently blocked but will be whitelisted.
			trackingKey := TrackingKey{IPInfo: NewIPInfo(tt.blockedIP)}

			// 3. Manually set the state in ActivityStore to simulate a blocked IP.
			processor.ActivityStore[trackingKey] = &BotActivity{
				IsBlocked:    true,
				BlockedUntil: time.Now().Add(time.Hour),
			}

			// 4. Set the WhitelistNets on the processor's config to include the blocked IP.
			_, ipNet, _ := net.ParseCIDR(tt.blockedIP + "/32")
			processor.Config.WhitelistNets = []*net.IPNet{ipNet}

			// --- Act ---
			processor.CheckAndRemoveWhitelistedBlocks()

			// --- Assert ---
			if len(commandsReceived) != 1 || commandsReceived[0] != tt.expectedCommand {
				t.Errorf("Expected unblock command '%s', but got %v", tt.expectedCommand, commandsReceived)
			}

			activity, exists := processor.ActivityStore[trackingKey]
			if !exists {
				t.Fatal("Activity for the IP was unexpectedly deleted.")
			}
			if activity.IsBlocked {
				t.Error("Expected IsBlocked to be false after whitelist cleanup, but it was true.")
			}
		})
	}
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
