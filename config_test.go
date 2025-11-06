package main

import (
	"os"
	"path/filepath"
	"strings"
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
	chains, err := LoadChainsFromYAML()

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

	// Check global state was updated
	ChainMutex.RLock()
	if len(WhitelistNets) != 4 {
		t.Errorf("Expected 4 whitelist CIDRs, got %d", len(WhitelistNets))
	}
	ChainMutex.RUnlock()

	HAProxyMutex.RLock()
	if len(HAProxyAddresses) != 1 {
		t.Errorf("Expected 1 HAProxy address, got %d", len(HAProxyAddresses))
	}
	HAProxyMutex.RUnlock()

	DurationTableMutex.RLock()
	if len(DurationToTableName) != 2 {
		t.Errorf("Expected 2 duration tables, got %d", len(DurationToTableName))
	}
	if BlockTableNameFallback != "table_1h" {
		t.Errorf("Expected fallback table 'table_1h', got '%s'", BlockTableNameFallback)
	}
	DurationTableMutex.RUnlock()
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

			_, err := LoadChainsFromYAML()

			if err == nil || !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing '%s', but got: %v", tt.expectedError, err)
			}
		})
	}
}
