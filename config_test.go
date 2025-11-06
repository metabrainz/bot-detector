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
	chains, whitelistNets, haProxyAddrs, durationTables, fallbackTable, err := LoadChainsFromYAML()

	// --- Assert ---
	if err != nil {
		t.Fatalf("LoadChainsFromYAML() returned an unexpected error: %v", err)
	}

	if len(chains) != 1 {
		t.Fatalf("Expected 1 chain to be loaded, got %d", len(chains))
	}

	if chains[0].Name != "TestChain" {
		t.Errorf("Expected chain name 'TestChain', got '%s'", chains[0].Name)
	}

	if chains[0].BlockDuration != 5*time.Minute {
		t.Errorf("Expected block duration 5m, got %v", chains[0].BlockDuration)
	}

	// Assertions for the new two-step chain structure
	if len(chains[0].Steps) != 2 {
		t.Fatalf("Expected chain to have 2 steps, got %d", len(chains[0].Steps))
	}

	step1 := chains[0].Steps[0]
	if step1.Order != 1 {
		t.Errorf("Expected step 1 to have order 1, got %d", step1.Order)
	}

	step2 := chains[0].Steps[1]
	if step2.Order != 2 {
		t.Errorf("Expected step 2 to have order 2, got %d", step2.Order)
	}
	if step2.MinDelayDuration != 1*time.Second {
		t.Errorf("Expected step 2 to have min_delay of 1s, got %v", step2.MinDelayDuration)
	}

	// Check other returned config values
	if len(whitelistNets) != 4 {
		t.Errorf("Expected 4 whitelist CIDRs, got %d", len(whitelistNets))
	}

	if len(haProxyAddrs) != 1 {
		t.Errorf("Expected 1 HAProxy address, got %d", len(haProxyAddrs))
	}

	if len(durationTables) != 2 {
		t.Errorf("Expected 2 duration tables, got %d", len(durationTables))
	}
	if fallbackTable != "table_1h" {
		t.Errorf("Expected fallback table 'table_1h', got '%s'", fallbackTable)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			tempFile := filepath.Join(tempDir, "chains.yaml")
			os.WriteFile(tempFile, []byte(tt.yamlContent), 0644)

			originalPath := YAMLFilePath
			YAMLFilePath = tempFile
			t.Cleanup(func() { YAMLFilePath = originalPath })

			_, _, _, _, _, err := LoadChainsFromYAML()

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
