package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadConfigFromYAML_Success(t *testing.T) {
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
	setupTestYAML(t, yamlContent)
	t.Cleanup(resetGlobalState)

	// --- Act ---
	loadedCfg, err := LoadConfigFromYAML() // Now returns *LoadedConfig, error

	// --- Assert ---
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() returned an unexpected error: %v", err)
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

func setupTestYAML(t *testing.T, content string) {
	t.Helper() // Mark this as a test helper function.

	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "chains.yaml")
	if err := os.WriteFile(tempFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write temp yaml file: %v", err)
	}

	originalPath := YAMLFilePath
	YAMLFilePath = tempFile
	t.Cleanup(func() {
		YAMLFilePath = originalPath
	})
}

func TestLoadConfigFromYAML_HAProxySettings(t *testing.T) {
	tests := []struct {
		name                string
		yamlContent         string
		expectedMaxRetries  int
		expectedRetryDelay  time.Duration
		expectedDialTimeout time.Duration
		expectError         bool
	}{
		{
			name: "Custom values",
			yamlContent: `
version: "1.0"
haproxy_max_retries: 5
haproxy_retry_delay: "300ms"
haproxy_dial_timeout: "10s"
`,
			expectedMaxRetries:  5,
			expectedRetryDelay:  300 * time.Millisecond,
			expectedDialTimeout: 10 * time.Second,
		},
		{
			name: "Default values",
			yamlContent: `
version: "1.0"
`,
			expectedMaxRetries:  DefaultHAProxyMaxRetries,
			expectedRetryDelay:  DefaultHAProxyRetryDelay,
			expectedDialTimeout: DefaultHAProxyDialTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestYAML(t, tt.yamlContent)
			loadedCfg, err := LoadConfigFromYAML()
			if err != nil {
				t.Fatalf("LoadConfigFromYAML() failed: %v", err)
			}
			if loadedCfg.HAProxyMaxRetries != tt.expectedMaxRetries || loadedCfg.HAProxyRetryDelay != tt.expectedRetryDelay || loadedCfg.HAProxyDialTimeout != tt.expectedDialTimeout {
				t.Errorf("HAProxy settings mismatch. Got retries=%d, delay=%v, timeout=%v. Expected retries=%d, delay=%v, timeout=%v", loadedCfg.HAProxyMaxRetries, loadedCfg.HAProxyRetryDelay, loadedCfg.HAProxyDialTimeout, tt.expectedMaxRetries, tt.expectedRetryDelay, tt.expectedDialTimeout)
			}
		})
	}
}

func TestLoadConfigFromYAML_ObjectMatcher(t *testing.T) {
	// --- Setup ---
	yamlContent := `
version: "1.0"
chains:
  - name: "StatusCodeRangeChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          StatusCode:
            gte: 400
            lt: 500
`
	setupTestYAML(t, yamlContent)

	// --- Act ---
	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed: %v", err)
	}

	// --- Assert ---
	if len(loadedCfg.Chains) != 1 || len(loadedCfg.Chains[0].Steps) != 1 || len(loadedCfg.Chains[0].Steps[0].Matchers) != 1 {
		t.Fatal("Failed to load or compile the object matcher chain correctly.")
	}

	matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

	// Test cases
	testEntries := map[string]struct {
		entry    *LogEntry
		expected bool
	}{
		"In Range (404)":              {entry: &LogEntry{StatusCode: 404}, expected: true},
		"Boundary In Range (400)":     {entry: &LogEntry{StatusCode: 400}, expected: true},
		"Boundary Out of Range (500)": {entry: &LogEntry{StatusCode: 500}, expected: false},
		"Out of Range (200)":          {entry: &LogEntry{StatusCode: 200}, expected: false},
	}

	for name, tc := range testEntries {
		t.Run(name, func(t *testing.T) {
			if got := matcher(tc.entry); got != tc.expected {
				t.Errorf("Matcher returned %v, expected %v for status code %d", got, tc.expected, tc.entry.StatusCode)
			}
		})
	}
}

func TestLoadConfigFromYAML_ObjectMatcher_OtherOperators(t *testing.T) {
	// This test covers the 'gt' and 'lte' operators not covered by the main object matcher test.
	yamlContent := `
version: "1.0"
chains:
  - name: "StatusCodeRangeChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          StatusCode:
            gt: 400
            lte: 404
`
	setupTestYAML(t, yamlContent)
	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed: %v", err)
	}
	matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

	// Test cases
	if !matcher(&LogEntry{StatusCode: 404}) { // 404 <= 404 -> true
		t.Error("Matcher failed for lte boundary")
	}
	if matcher(&LogEntry{StatusCode: 400}) { // 400 > 400 -> false
		t.Error("Matcher failed for gt boundary")
	}
}

func TestLoadConfigFromYAML_ObjectMatcher_WithNot(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		testCases   map[string]struct {
			entry    *LogEntry
			expected bool
		}
		expectError string
	}{
		{
			name: "gte, lt, and not (single int)",
			yamlContent: `
version: "1.0"
chains:
  - name: "StatusCodeRangeNotChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          StatusCode:
            gte: 400
            lt: 500
            not: 404`,
			testCases: map[string]struct {
				entry    *LogEntry
				expected bool
			}{
				"In Range, Not Excluded (403)": {entry: &LogEntry{StatusCode: 403}, expected: true},
				"Excluded (404)":               {entry: &LogEntry{StatusCode: 404}, expected: false},
				"Out of Range (500)":           {entry: &LogEntry{StatusCode: 500}, expected: false},
			},
		},
		{
			name: "not with list of ints",
			yamlContent: `
version: "1.0"
chains:
  - name: "StatusCodeNotListChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          StatusCode:
            not: [403, 404]`,
			testCases: map[string]struct {
				entry    *LogEntry
				expected bool
			}{
				"Not in list (200)": {entry: &LogEntry{StatusCode: 200}, expected: true},
				"In list (403)":     {entry: &LogEntry{StatusCode: 403}, expected: false},
				"In list (404)":     {entry: &LogEntry{StatusCode: 404}, expected: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestYAML(t, tt.yamlContent)
			loadedCfg, err := LoadConfigFromYAML()
			if err != nil {
				t.Fatalf("LoadConfigFromYAML() failed: %v", err)
			}
			matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

			for name, tc := range tt.testCases {
				t.Run(name, func(t *testing.T) {
					if got := matcher(tc.entry); got != tc.expected {
						t.Errorf("Matcher returned %v, expected %v for status code %d", got, tc.expected, tc.entry.StatusCode)
					}
				})
			}
		})
	}
}

func TestLoadConfigFromYAML_IntMatcherFallback(t *testing.T) {
	// This test specifically targets the non-StatusCode path in compileIntMatcher.
	// We use a field that is not an integer (Method) to ensure the fallback logic
	// of converting a string value to an integer is tested.
	// --- Setup ---
	yamlContent := `
version: "1.0"
chains:
  - name: "IntFallbackChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          Method: 123 # Using an integer value on a string field
`
	setupTestYAML(t, yamlContent)

	// --- Act ---
	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed: %v", err)
	}

	// --- Assert ---
	if len(loadedCfg.Chains) != 1 || len(loadedCfg.Chains[0].Steps[0].Matchers) != 1 {
		t.Fatal("Failed to load or compile the int matcher fallback chain.")
	}

	matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

	// This entry should NOT match because "GET" != "123"
	if matcher(&LogEntry{Method: "GET"}) {
		t.Error("Matcher incorrectly matched 'GET' with integer 123.")
	}
}

func TestLoadConfigFromYAML_Errors(t *testing.T) {
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
			name: "Missing Version",
			yamlContent: `
chains: []
`,
			expectedError: "configuration file is missing the required 'version' field",
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
			name: "Invalid Regex",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "Path": "regex:/(" } } ]
`,
			expectedError: "invalid regex",
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
			name: "Block Action Missing Duration",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    action: "block" # No block_duration and no default
`,
			expectedError: "block_duration is missing or zero",
		},
		{
			name: "Object Matcher with Non-Integer Value",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "StatusCode": { gte: "400" } } } ]
`,
			expectedError: "value for 'gte' must be an integer",
		},
		{
			name: "Object Matcher with Unknown Operator",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "StatusCode": { eq: 400 } } } ]
`,
			expectedError: "unknown operator 'eq' in object matcher",
		},
		{
			name: "Object Matcher with Empty Object",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "StatusCode": {} } } ]
`,
			expectedError: "object matcher must not be empty",
		},
		{
			name: "Unsupported Matcher Type",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "StatusCode": true } } ]
`,
			expectedError: "unsupported value type 'bool'",
		},
		{
			name: "Invalid Item in List Matcher",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "Path": ["/good", { "gt": 1 }] } } ]
`,
			expectedError: "object matchers (gte, lt, etc.) are only supported for the 'StatusCode' field, not 'Path'",
		},
		{
			name: "File Matcher Not Found (Non-Fatal)",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "Path": "file:/path/to/nonexistent/file.txt" } } ]
`,
			expectedError: "", // Should NOT produce a fatal error
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "File Not Found" {
				// For this specific test, ensure the file does not exist.
				YAMLFilePath = filepath.Join(t.TempDir(), "nonexistent.yaml")
			} else {
				setupTestYAML(t, tt.yamlContent)
			}

			// For tests that expect non-fatal errors, we can suppress the log output
			// to keep the test runner output clean.
			if tt.name == "File Matcher Not Found (Non-Fatal)" {
				originalLogFunc := logging.LogOutput
				logging.LogOutput = func(level logging.LogLevel, tag string, format string, args ...interface{}) {}
				t.Cleanup(func() { logging.LogOutput = originalLogFunc })
			}

			_, err := LoadConfigFromYAML()

			if tt.expectedError == "" {
				if err != nil {
					t.Errorf("Expected no error, but got: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing '%s', but got: %v", tt.expectedError, err)
			}
		})
	}
}

func TestLoadConfigFromYAML_InvalidDurations(t *testing.T) {
	tests := []struct {
		name          string
		yamlContent   string
		expectedError string
	}{
		{
			name: "Invalid poll_interval",
			yamlContent: `
version: "1.0"
poll_interval: "5x"`,
			expectedError: "invalid poll_interval format",
		},
		{
			name: "Invalid cleanup_interval",
			yamlContent: `
version: "1.0"
cleanup_interval: "1y"`,
			expectedError: "invalid cleanup_interval format",
		},
		{
			name: "Invalid idle_timeout",
			yamlContent: `
version: "1.0"
idle_timeout: "30z"`,
			expectedError: "invalid idle_timeout format",
		},
		{
			name: "Invalid out_of_order_tolerance",
			yamlContent: `
version: "1.0"
out_of_order_tolerance: "bad"`,
			expectedError: "invalid out_of_order_tolerance format",
		},
		{
			name: "Invalid eof_polling_delay",
			yamlContent: `
version: "1.0"
eof_polling_delay: "200"`,
			expectedError: "invalid eof_polling_delay format",
		},
	}
	// Add chain-specific duration tests
	tests = append(tests, []struct {
		name          string
		yamlContent   string
		expectedError string
	}{
		{
			name: "Invalid block_duration in chain",
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
			name: "Invalid duration in duration_tables",
			yamlContent: `
version: "1.0"
duration_tables:
  "1x": "table_1x"
`,
			expectedError: "invalid duration",
		},
		{
			name: "Invalid max_delay in step",
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
			name: "Invalid min_delay in step",
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
			name: "Invalid min_time_since_last_hit in step",
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
			name: "Invalid default_block_duration",
			yamlContent: `
version: "1.0"
default_block_duration: "1x"
chains: []
`,
			expectedError: "invalid block_duration format for default_block_duration",
		},
		{
			name: "Invalid haproxy_retry_delay",
			yamlContent: `
version: "1.0"
haproxy_retry_delay: "bad"`,
			expectedError: "invalid haproxy_retry_delay",
		},
		{
			name: "Invalid haproxy_dial_timeout",
			yamlContent: `
version: "1.0"
haproxy_dial_timeout: "1y"`,
			expectedError: "invalid haproxy_dial_timeout",
		},
	}...)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestYAML(t, tt.yamlContent)
			_, err := LoadConfigFromYAML()
			if err == nil || !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing '%s', but got: %v", tt.expectedError, err)
			}
		})
	}
}

func TestLoadConfigFromYAML_MissingOptionalCaptureGroup(t *testing.T) {
	// This test verifies that a custom regex is valid even if it's missing
	// optional capture groups like 'Referrer'.

	// Regex without the 'Referrer' named capture group.
	yamlContent := `
version: "1.0"
log_format_regex: '^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{1,3}) \d+ \"[^\"]*\" \"(?P<UserAgent>[^\"]*)\"$'
chains: []
`
	setupTestYAML(t, yamlContent)

	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed unexpectedly: %v", err)
	}

	if loadedCfg.LogFormatRegex == nil {
		t.Fatal("Expected LogFormatRegex to be loaded, but it was nil.")
	}

	// Now, test the parsing with this regex.
	p := &Processor{
		LogRegex: loadedCfg.LogFormatRegex,
		Config:   &AppConfig{TimestampFormat: loadedCfg.TimestampFormat},
	}
	logLine := `example.com 192.0.2.1 - - [01/Jan/2025:12:00:00 +0000] "GET /path HTTP/1.1" 200 123 "http://a.real.referrer" "TestAgent/1.0"`

	entry, err := ParseLogLine(p, logLine)
	if err != nil {
		t.Fatalf("ParseLogLine failed with a valid regex missing an optional group: %v", err)
	}

	if entry.Referrer != "" {
		t.Errorf("Expected Referrer to be an empty string since it was not in the regex, but got: '%s'", entry.Referrer)
	}
	if entry.UserAgent != "TestAgent/1.0" {
		t.Errorf("Expected UserAgent to be parsed correctly, but got: '%s'", entry.UserAgent)
	}
}

func TestLoadConfigFromYAML_MissingRequiredCaptureGroup(t *testing.T) {
	// This test verifies that a custom regex fails to load if it's missing
	// a required capture group like 'IP'.
	yamlContent := `
version: "1.0"
log_format_regex: '^(?P<VHost>\S+) \S+ \S+ \S+ \[(?P<Timestamp>[^\]]+)\] ".+"$'
chains: []
`
	setupTestYAML(t, yamlContent)

	_, err := LoadConfigFromYAML()
	if err == nil {
		t.Fatal("Expected an error when loading regex with missing required capture group, but got nil.")
	}
	if !strings.Contains(err.Error(), "missing required named capture group '(?P<IP>...)'") {
		t.Errorf("Expected error about missing 'IP' group, but got: %v", err)
	}
}

func TestLoadConfigFromYAML_CustomTimestampFormat(t *testing.T) {
	// This test verifies that a custom timestamp_format is correctly loaded and used.
	yamlContent := fmt.Sprintf(`
version: "1.0"
timestamp_format: "%s" # Use RFC3339 for this test
log_format_regex: '^(?P<IP>\S+) \[(?P<Timestamp>[^\]]+)\] (?P<Path>\S+)$'
chains: []
`, time.RFC3339)

	setupTestYAML(t, yamlContent)

	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("LoadConfigFromYAML() failed unexpectedly: %v", err)
	}

	if loadedCfg.TimestampFormat != time.RFC3339 {
		t.Fatalf("Expected TimestampFormat to be loaded as RFC3339, but got: '%s'", loadedCfg.TimestampFormat)
	}

	// Now, test the parsing with this config.
	p := &Processor{
		LogRegex: loadedCfg.LogFormatRegex,
		Config:   &AppConfig{TimestampFormat: loadedCfg.TimestampFormat},
	}
	logLine := `192.0.2.1 [2025-01-01T12:00:00Z] /test`

	entry, err := ParseLogLine(p, logLine)
	if err != nil {
		t.Fatalf("ParseLogLine failed with a custom timestamp format: %v", err)
	}

	expectedTime, _ := time.Parse(time.RFC3339, "2025-01-01T12:00:00Z")
	if !entry.Timestamp.Equal(expectedTime) {
		t.Errorf("Expected parsed timestamp to be %v, but got: %v", expectedTime, entry.Timestamp)
	}
	if entry.Path != "/test" {
		t.Errorf("Expected path to be /test, but got: %s", entry.Path)
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

			var commandsReceived []string
			// Create a processor instance for the test.
			processor := &Processor{
				ActivityStore: make(map[TrackingKey]*BotActivity),
				ActivityMutex: &sync.RWMutex{},
				ConfigMutex:   &sync.RWMutex{},
				Config: &AppConfig{
					// This config is needed for the p.UnblockIP call to work.
					HAProxyAddresses:    []string{"127.0.0.1:9999"},
					DurationToTableName: map[time.Duration]string{5 * time.Minute: "table_5m"},
				},
				// Capture log output for assertion.
				LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {
					// For this test, we only care about the WHITELIST_UNBLOCK log.
				},
				// Set the Blocker to a mock that delegates to the original UnblockIP method,
				// which in turn uses the mocked CommandExecutor below.
				Blocker: &HAProxyBlocker{P: &Processor{}}, // Temporary processor for delegation
			}
			// Now, set up the mock blocker correctly.
			mockBlocker := &HAProxyBlocker{P: processor}
			processor.Blocker = mockBlocker
			// Mock the underlying executor that the real UnblockIP method will call.
			processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
				commandsReceived = append(commandsReceived, strings.TrimSpace(command))
				return nil
			}

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
			processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
				if tag == "WHITELIST_UNBLOCK" {
					capturedLog = fmt.Sprintf(format, args...)
				}
			}

			// 4. Set the WhitelistNets on the processor's config to include the blocked IP.
			_, ipNet, _ := net.ParseCIDR(tt.blockedIP + "/32")
			processor.Config.WhitelistNets = []*net.IPNet{ipNet}

			// --- Act ---
			CheckAndRemoveWhitelistedBlocks(processor)

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

		var unblockCalled bool
		processor := &Processor{
			ActivityMutex: &sync.RWMutex{},
			ActivityStore: make(map[TrackingKey]*BotActivity),
			ConfigMutex:   &sync.RWMutex{},
			Config: &AppConfig{
				HAProxyAddresses:    []string{"127.0.0.1:9999"},
				DurationToTableName: map[time.Duration]string{time.Minute: "t1"},
			},
			LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
			// Mock the Blocker to simulate a failure.
			Blocker: &MockBlocker{
				UnblockFunc: func(ipInfo IPInfo) error {
					unblockCalled = true
					return fmt.Errorf("simulated HAProxy failure")
				},
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
		CheckAndRemoveWhitelistedBlocks(processor)

		// --- Assert ---
		// The IP should remain blocked in memory because the HAProxy command failed.
		if !processor.ActivityStore[trackingKey].IsBlocked {
			t.Error("Expected IsBlocked to remain true after a failed unblock attempt, but it was set to false.")
		}
		if !unblockCalled {
			t.Error("Expected the mock Blocker.Unblock method to be called, but it was not.")
		}
	})

	t.Run("No Action Needed", func(t *testing.T) {
		// This test covers the nil-function path in the MockBlocker.
		// --- Setup ---
		resetGlobalState()

		processor := &Processor{
			ActivityMutex: &sync.RWMutex{},
			ActivityStore: make(map[TrackingKey]*BotActivity),
			Config:        &AppConfig{},
			LogFunc:       func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
			// Use a MockBlocker but do NOT set the UnblockFunc.
			Blocker: &MockBlocker{},
		}

		// Manually set a blocked IP that is NOT on the whitelist.
		blockedIP := "192.0.2.100"
		trackingKey := TrackingKey{IPInfo: NewIPInfo(blockedIP)}
		processor.ActivityStore[trackingKey] = &BotActivity{
			IsBlocked:    true,
			BlockedUntil: time.Now().Add(time.Hour),
		}
		// The whitelist is empty, so no IPs should be unblocked.
		processor.Config.WhitelistNets = []*net.IPNet{}

		// --- Act ---
		CheckAndRemoveWhitelistedBlocks(processor)

		// --- Assert ---
		// The main assertion is that the test completes without panic.
		// Calling Blocker.Unblock() on the mock will execute the `return nil` path.
	})
}

func TestConfigWatcher_Reload(t *testing.T) {
	// --- Setup ---
	// This test involves loading configs which can be noisy.
	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

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
	initialLoadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config.
	processor := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        initialLoadedCfg.Chains,
		Config:        &AppConfig{PollingInterval: 10 * time.Millisecond},
		LogFunc:       func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
	}
	// Set LastModTime to the actual modification time of the initial file.
	initialFileInfo, err := os.Stat(tempFile)
	if err != nil {
		t.Fatalf("Failed to stat initial temp yaml file: %v", err)
	}
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 4. Start the ConfigWatcher with the test signal channel.
	forceCheckSignal := make(chan struct{}, 1)
	reloadDoneSignal := make(chan struct{}, 1)
	stopWatcher := make(chan struct{})
	go ConfigWatcher(processor, stopWatcher, forceCheckSignal, reloadDoneSignal)

	// --- Act ---
	// 5. Modify the YAML file on disk.
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
	// Manually advance the file's modification time to guarantee the watcher sees a change.
	// This is faster and more reliable than time.Sleep().
	futureTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(tempFile, futureTime, futureTime); err != nil {
		t.Fatalf("Failed to change file modification time: %v", err)
	}

	// 6. Force the watcher to check immediately.
	forceCheckSignal <- struct{}{}

	// 7. Wait for the reload signal from the watcher.
	select {
	case <-reloadDoneSignal:
		// Reload completed successfully.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for configuration reload.")
	}

	// --- Assert ---
	// 8. Check if the processor's state has been updated.
	processor.ConfigMutex.RLock()
	defer processor.ConfigMutex.RUnlock()

	if len(processor.Chains) != 1 || processor.Chains[0].Name != "ReloadedChain" {
		t.Errorf("Expected chain to be 'ReloadedChain', but got: %+v", processor.Chains)
	}
	if len(processor.Config.WhitelistNets) != 2 {
		t.Errorf("Expected 2 whitelist networks, but got %d", len(processor.Config.WhitelistNets))
	}
	if logging.GetLogLevel() != logging.LevelDebug {
		t.Errorf("Expected log level to be updated to 'debug', but it was not.")
	}
}

func TestConfigWatcher_FileDependencyReload(t *testing.T) {
	// --- Setup ---
	tempDir := t.TempDir()

	// 1. Create the initial dependency file (bad_agents.txt)
	agentFilePath := filepath.Join(tempDir, "bad_agents.txt")
	if err := os.WriteFile(agentFilePath, []byte("InitialBadAgent/1.0"), 0644); err != nil {
		t.Fatalf("Failed to write initial agent file: %v", err)
	}

	// 2. Create the initial YAML file that references the dependency
	initialYAMLContent := fmt.Sprintf(`
version: "1.0"
chains:
  - name: "FileWatcherChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          UserAgent: "file:%s"
`, agentFilePath)

	tempYamlFile := filepath.Join(tempDir, "chains.yaml")
	if err := os.WriteFile(tempYamlFile, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}

	// Point the global YAMLFilePath to our temp file for the duration of the test.
	originalPath := YAMLFilePath
	YAMLFilePath = tempYamlFile
	t.Cleanup(func() { YAMLFilePath = originalPath })

	// 3. Load the initial configuration.
	initialLoadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML() failed: %v", err)
	}

	// 4. Create the processor with the initial config.
	processor := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        initialLoadedCfg.Chains,
		Config: &AppConfig{
			FileDependencies: initialLoadedCfg.FileDependencies,
			PollingInterval:  10 * time.Millisecond,
		},
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
	}
	initialFileInfo, _ := os.Stat(tempYamlFile)
	processor.Config.LastModTime = initialFileInfo.ModTime() // Set initial mod time

	// 5. Start the ConfigWatcher with the test signal channel.
	forceCheckSignal := make(chan struct{}, 1)
	reloadDoneSignal := make(chan struct{}, 1)
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go ConfigWatcher(processor, stopWatcher, forceCheckSignal, reloadDoneSignal)
	// --- Act ---
	// 6. Modify ONLY the dependency file.
	if err := os.WriteFile(agentFilePath, []byte("ReloadedBadAgent/2.0"), 0644); err != nil {
		t.Fatalf("Failed to write modified agent file: %v", err)
	}
	// Manually advance the dependency file's modification time.
	futureTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(agentFilePath, futureTime, futureTime); err != nil {
		t.Fatalf("Failed to change file modification time: %v", err)
	}

	// 7. Force the watcher to check immediately.
	forceCheckSignal <- struct{}{}

	// 8. Wait for the reload signal from the watcher.
	select {
	case <-reloadDoneSignal:
		// Reload completed successfully.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for configuration reload.")
	}

	// --- Assert ---
	// 9. Check if the processor's internal matchers have been updated.
	processor.ConfigMutex.RLock()
	defer processor.ConfigMutex.RUnlock()

	if len(processor.Chains) != 1 || len(processor.Chains[0].Steps) != 1 {
		t.Fatal("Processor chains were not reloaded correctly.")
	}

	// Create log entries to test the old and new rules.
	entryWithOldAgent := &LogEntry{UserAgent: "InitialBadAgent/1.0"}
	entryWithNewAgent := &LogEntry{UserAgent: "ReloadedBadAgent/2.0"}

	// The single matcher function should now only match the new agent.
	matcherFunc := processor.Chains[0].Steps[0].Matchers[0]
	if matcherFunc(entryWithOldAgent) || !matcherFunc(entryWithNewAgent) {
		t.Error("The file-based matcher was not updated correctly after the dependency file was reloaded.")
	}
}

func TestConfigWatcher_ReloadFailure(t *testing.T) {
	// --- Setup ---
	// This test involves loading configs which can be noisy.
	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

	// 1. Create a temporary YAML file with initial valid content.
	initialYAMLContent := `
version: "1.0"
log_level: "info"
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
	initialLoadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config and a log capturer.
	var capturedLogs []string
	var logMutex sync.Mutex
	processor := &Processor{
		ConfigMutex: &sync.RWMutex{},
		Chains:      initialLoadedCfg.Chains,
		Config:      &AppConfig{PollingInterval: 10 * time.Millisecond},
	}
	initialFileInfo, _ := os.Stat(tempFile)
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 4. Set up the channel-signaling LogFunc BEFORE starting the goroutine.
	logReceived := make(chan bool, 1)
	processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
		logMutex.Unlock()
		if tag == "LOAD_ERROR" {
			logReceived <- true
		}
	}

	// 5. Start the ConfigWatcher.
	forceCheckSignal := make(chan struct{}, 1)
	reloadDoneSignal := make(chan struct{}, 1)
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go ConfigWatcher(processor, stopWatcher, forceCheckSignal, reloadDoneSignal)

	// --- Act ---
	// 6. Modify the YAML file with INVALID content.
	// The YAML must be syntactically valid, but logically incorrect (bad regex).
	// Using a multi-line string is the correct way to format this.
	invalidYAMLContent := `
version: "1.0"
chains:
  - name: "InvalidRegexChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "regex:("}}]
`
	if err := os.WriteFile(tempFile, []byte(invalidYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write invalid YAML: %v", err)
	}
	// Manually advance the file's modification time.
	futureTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(tempFile, futureTime, futureTime); err != nil {
		t.Fatalf("Failed to change file modification time: %v", err)
	}

	// 7. Force the watcher to perform a check immediately, bypassing the timer.
	forceCheckSignal <- struct{}{}

	// 8. Wait for the watcher to log the error. This is now deterministic because
	// we triggered the check manually.
	select {
	case <-logReceived:
		// The LOAD_ERROR was logged as expected.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for LOAD_ERROR log message.")
	}

	// 9. Check that the error was logged.
	// The fact that we received a signal on logReceived is sufficient proof.
	// We can add a redundant check on the captured logs for extra safety.
	logMutex.Lock()
	defer logMutex.Unlock()
	if len(capturedLogs) == 0 || !strings.Contains(capturedLogs[len(capturedLogs)-1], "LOAD_ERROR") {
		t.Errorf("Expected a 'LOAD_ERROR' log message, but none was found. Logs: %v", capturedLogs)
	}
}

func TestConfigWatcher_StatError(t *testing.T) {
	// --- Setup ---
	// This test involves loading configs which can be noisy.
	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

	// 1. Create a temporary YAML file.
	initialYAMLContent := `
version: "1.0"
log_level: "info"
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
		ConfigMutex: &sync.RWMutex{},
		Chains:      []BehavioralChain{{Name: "InitialChain"}}, // Simplified initial state
		Config:      &AppConfig{PollingInterval: 10 * time.Millisecond},
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {
			logMutex.Lock()
			capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
			logMutex.Unlock()
		},
	}
	initialFileInfo, _ := os.Stat(tempFile)
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 3. Set up log capture BEFORE starting the watcher to avoid a race condition.
	logReceived := make(chan bool, 1)
	processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
		logMutex.Unlock()
		if tag == "WATCH_ERROR" {
			logReceived <- true
		}
	}
	forceCheckSignal := make(chan struct{}, 1)
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go ConfigWatcher(processor, stopWatcher, forceCheckSignal, nil)

	// --- Act ---
	// 4. Delete the YAML file to trigger a stat error on the next poll.
	// We add a small sleep to ensure the watcher has started and is ready.
	time.Sleep(50 * time.Millisecond)
	if err := os.Remove(tempFile); err != nil {
		t.Fatalf("Failed to remove temp file: %v", err)
	}

	// 5. Force the watcher to check immediately.
	forceCheckSignal <- struct{}{}

	// 6. Wait for the watcher to log the error.
	select {
	case <-logReceived:
		// Error was logged as expected.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for WATCH_ERROR log message.")
	}

	// --- Assert ---
	// 7. Check that the correct error was logged.
	logMutex.Lock()
	logOutput := strings.Join(capturedLogs, "\n")
	logMutex.Unlock()
	if !strings.Contains(logOutput, "WATCH_ERROR: Failed to stat file") {
		t.Errorf("Expected a 'WATCH_ERROR' log message, but none was found. Logs:\n%s", logOutput)
	}
}
