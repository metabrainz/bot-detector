package config_test

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/testutil"
	"bot-detector/internal/logging"
	"bot-detector/internal/types"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/parser"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"fmt"

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

blockers:
  backends:
    haproxy:
      addresses:
        - "127.0.0.1:9999"
      duration_tables:
        "5m": "table_5m"
        "1h": "table_1h"
  default_duration: "1h"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "block"
    block_duration: "5m"
    steps:
      - max_delay: "10s" # Step 1
        field_matches:
          path: "/login"
      - min_delay: "1s" # Step 2
        field_matches:
          path: "/login/confirm"
  - name: "TestDefaultDurationChain"
    match_key: "ip"
    action: "block" # No block_duration, should use default
    steps:
      - field_matches: { path: "/default" }
`
	tmpConfigPath := setupTestYAML(t, yamlContent)
	t.Cleanup(testutil.ResetGlobalState)

	// --- Act ---
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})

	// --- Assert ---
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() returned an unexpected error: %v", err)
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

	if loadedCfg.Chains[0].BlockDurationStr != "5m" {
		t.Errorf("Expected block duration string '5m', got '%s'", loadedCfg.Chains[0].BlockDurationStr)
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

}

func setupTestYAML(t *testing.T, content string) string {
	t.Helper() // Mark this as a test helper function.

	tempDir := t.TempDir()
	tmpConfigPath := filepath.Join(tempDir, "config test.yaml")
	if err := os.WriteFile(tmpConfigPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write temp yaml file: %v", err)
	}

	return tmpConfigPath
}

func TestLoadConfigFromYAML_BlockerSettings(t *testing.T) {
	tests := []struct {
		name                      string
		yamlContent               string
		expectedMaxRetries        int
		expectedRetryDelay        time.Duration
		expectedDialTimeout       time.Duration
		expectedCommandQueueSize  int
		expectedCommandsPerSecond int
		expectError               bool
	}{
		{
			name: "Custom values",
			yamlContent: `
version: "1.0"
blockers:
  max_retries: 5
  retry_delay: "300ms"
  dial_timeout: "10s"
  command_queue_size: 500
  commands_per_second: 5
`,
			expectedMaxRetries:        5,
			expectedRetryDelay:        300 * time.Millisecond,
			expectedDialTimeout:       10 * time.Second,
			expectedCommandQueueSize:  500,
			expectedCommandsPerSecond: 5,
		},
		{
			name: "Default values",
			yamlContent: `
version: "1.0"
blockers: {}
`,
			expectedMaxRetries:        config.DefaultBlockerMaxRetries,
			expectedRetryDelay:        config.DefaultBlockerRetryDelay,
			expectedDialTimeout:       config.DefaultBlockerDialTimeout,
			expectedCommandQueueSize:  config.DefaultBlockerCommandQueueSize,
			expectedCommandsPerSecond: config.DefaultBlockerCommandsPerSecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpConfigPath := setupTestYAML(t, tt.yamlContent)
			loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
			if err != nil {
				t.Fatalf("config.LoadConfigFromYAML() failed: %v", err)
			}
			if loadedCfg.Blockers.MaxRetries != tt.expectedMaxRetries ||
				loadedCfg.Blockers.RetryDelay != tt.expectedRetryDelay ||
				loadedCfg.Blockers.DialTimeout != tt.expectedDialTimeout ||
				loadedCfg.Blockers.CommandQueueSize != tt.expectedCommandQueueSize ||
				loadedCfg.Blockers.CommandsPerSecond != tt.expectedCommandsPerSecond {
				t.Errorf("Blocker settings mismatch. Got retries=%d, delay=%v, timeout=%v, queue_size=%d, commands_per_second=%d. Expected retries=%d, delay=%v, timeout=%v, queue_size=%d, commands_per_second=%d",
					loadedCfg.Blockers.MaxRetries, loadedCfg.Blockers.RetryDelay, loadedCfg.Blockers.DialTimeout, loadedCfg.Blockers.CommandQueueSize, loadedCfg.Blockers.CommandsPerSecond,
					tt.expectedMaxRetries, tt.expectedRetryDelay, tt.expectedDialTimeout, tt.expectedCommandQueueSize, tt.expectedCommandsPerSecond)
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
          statuscode:
            gt: 400
            lte: 404
`
	tmpConfigPath := setupTestYAML(t, yamlContent)
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() failed: %v", err)
	}
	matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

	// Test cases
	if !matcher.Matcher(&types.LogEntry{StatusCode: 404}) { // 404 <= 404 -> true
		t.Error("Matcher failed for lte boundary")
	}
	if matcher.Matcher(&types.LogEntry{StatusCode: 400}) { // 400 > 400 -> false
		t.Error("Matcher failed for gt boundary")
	}
}

func TestLoadConfigFromYAML_ObjectMatcher(t *testing.T) {
	yamlContent := `
version: "1.0"
chains:
  - name: "StatusCodeRangeChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          statuscode:
            gte: 400
            lt: 500
`
	testCases := map[string]struct {
		entry    *types.LogEntry
		expected bool
	}{
		"In Range (404)":              {entry: &types.LogEntry{StatusCode: 404}, expected: true},
		"Boundary In Range (400)":     {entry: &types.LogEntry{StatusCode: 400}, expected: true},
		"Boundary Out of Range (500)": {entry: &types.LogEntry{StatusCode: 500}, expected: false},
		"Out of Range (200)":          {entry: &types.LogEntry{StatusCode: 200}, expected: false},
	}

	runMatcherTest(t, yamlContent, testCases, "")
}

func TestLoadConfigFromYAML_ObjectMatcher_Size(t *testing.T) {
	yamlContent := `
version: "1.0"
chains:
  - name: "SizeRangeChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          size:
            gte: 1000
            lt: 2000
`
	testCases := map[string]struct {
		entry    *types.LogEntry
		expected bool
	}{
		"In Range (1500)":              {entry: &types.LogEntry{Size: 1500}, expected: true},
		"Boundary In Range (1000)":     {entry: &types.LogEntry{Size: 1000}, expected: true},
		"Boundary Out of Range (2000)": {entry: &types.LogEntry{Size: 2000}, expected: false},
		"Out of Range (500)":           {entry: &types.LogEntry{Size: 500}, expected: false},
	}

	runMatcherTest(t, yamlContent, testCases, "")
}

func TestLoadConfigFromYAML_ObjectMatcher_WithNot(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		testCases   map[string]struct {
			entry    *types.LogEntry
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
          statuscode:
            gte: 400
            lt: 500
            not: 404`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"In Range, Not Excluded (403)": {entry: &types.LogEntry{StatusCode: 403}, expected: true},
				"Excluded (404)":               {entry: &types.LogEntry{StatusCode: 404}, expected: false},
				"Out of Range (500)":           {entry: &types.LogEntry{StatusCode: 500}, expected: false},
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
          statuscode:
            not: [403, 404]`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Not in list (200)": {entry: &types.LogEntry{StatusCode: 200}, expected: true},
				"In list (403)":     {entry: &types.LogEntry{StatusCode: 403}, expected: false},
				"In list (404)":     {entry: &types.LogEntry{StatusCode: 404}, expected: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runMatcherTest(t, tt.yamlContent, tt.testCases, "")
		})
	}
}

func TestLoadConfigFromYAML_ObjectMatcher_Not_WithPath(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		testCases   map[string]struct {
			entry    *types.LogEntry
			expected bool
		}
	}{
		{
			name: "not with single regex path",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path:
            not: "regex:^/admin"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches other path":           {entry: &types.LogEntry{Path: "/dashboard"}, expected: true},
				"Does not match excluded path": {entry: &types.LogEntry{Path: "/admin"}, expected: false},
				"Does not match sub-path":      {entry: &types.LogEntry{Path: "/admin/users"}, expected: false},
			},
		},
		{
			name: "not with list of strings and regex",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path:
            not:
              - "/admin"
              - "regex:^/api/v1/public/"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches other path":                 {entry: &types.LogEntry{Path: "/dashboard"}, expected: true},
				"Does not match exact excluded path": {entry: &types.LogEntry{Path: "/admin"}, expected: false},
				"Does not match regex excluded path": {entry: &types.LogEntry{Path: "/api/v1/public/users"}, expected: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runMatcherTest(t, tt.yamlContent, tt.testCases, "")
		})
	}
}

func TestLoadConfigFromYAML_StringMatcher_Prefixes(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		testCases   map[string]struct {
			entry    *types.LogEntry
			expected bool
		}
	}{
		{
			name: "exact prefix to match literal 'regex:'",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "exact:regex:"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches literal 'regex:'": {entry: &types.LogEntry{Path: "regex:"}, expected: true},
				"Does not match other":     {entry: &types.LogEntry{Path: "/path"}, expected: false},
			},
		},
		{
			name: "literal 'regex:' without prefix",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "regex:"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches literal 'regex:'": {entry: &types.LogEntry{Path: "regex:"}, expected: true},
				"Does not match other":     {entry: &types.LogEntry{Path: "/path"}, expected: false},
			},
		},
		{
			name: "literal 'file:' without prefix",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "file:"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches literal 'file:'": {entry: &types.LogEntry{Path: "file:"}, expected: true},
				"Does not match other":    {entry: &types.LogEntry{Path: "/path"}, expected: false},
			},
		},
		{
			name: "normal regex still works",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "regex:^/test"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches regex":          {entry: &types.LogEntry{Path: "/test/path"}, expected: true},
				"Does not match":         {entry: &types.LogEntry{Path: "/other"}, expected: false},
				"Does not match literal": {entry: &types.LogEntry{Path: "regex:^/test"}, expected: false},
			},
		},
		{
			name: "status code pattern matcher",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          statuscode: "4XX"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches 404":        {entry: &types.LogEntry{StatusCode: 404}, expected: true},
				"Matches 400":        {entry: &types.LogEntry{StatusCode: 400}, expected: true},
				"Matches 499":        {entry: &types.LogEntry{StatusCode: 499}, expected: true},
				"Does not match 500": {entry: &types.LogEntry{StatusCode: 500}, expected: false},
				"Does not match 399": {entry: &types.LogEntry{StatusCode: 399}, expected: false},
			},
		},
		{
			name: "status code pattern matcher 30X",
			yamlContent: `
version: "1.0"
chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          statuscode: "30X"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"Matches 301":        {entry: &types.LogEntry{StatusCode: 301}, expected: true},
				"Matches 302":        {entry: &types.LogEntry{StatusCode: 302}, expected: true},
				"Matches 309":        {entry: &types.LogEntry{StatusCode: 309}, expected: true},
				"Does not match 310": {entry: &types.LogEntry{StatusCode: 310}, expected: false},
				"Does not match 299": {entry: &types.LogEntry{StatusCode: 299}, expected: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runMatcherTest(t, tt.yamlContent, tt.testCases, "")
		})
	}
}

func TestLoadConfigFromYAML_CIDRMatcher(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		testCases   map[string]struct {
			entry    *types.LogEntry
			expected bool
		}
		expectError string
	}{
		{
			name: "IPv4 CIDR Matcher",
			yamlContent: `
version: "1.0"
chains:
  - name: "CIDRTest"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          ip: "cidr:192.168.1.0/24"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"IP in range":      {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("192.168.1.100")}, expected: true},
				"IP not in range":  {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("192.168.2.1")}, expected: false},
				"IP is network":    {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("192.168.1.0")}, expected: true},
				"IP is broadcast":  {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("192.168.1.255")}, expected: true},
				"Invalid entry IP": {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("not-an-ip")}, expected: false},
			},
		},
		{
			name: "IPv6 CIDR Matcher",
			yamlContent: `
version: "1.0"
chains:
  - name: "CIDRTestIPv6"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          ip: "cidr:2001:db8:abcd:0012::/64"`,
			testCases: map[string]struct {
				entry    *types.LogEntry
				expected bool
			}{
				"IPv6 in range":       {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("2001:db8:abcd:0012:dead:beef:cafe:babe")}, expected: true},
				"IPv6 not in range":   {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("2001:db8:abcd:0013::1")}, expected: false},
				"IPv6 is network":     {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("2001:db8:abcd:0012::")}, expected: true},
				"IPv4 does not match": {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("192.168.1.1")}, expected: false},
				"Invalid entry IP":    {entry: &types.LogEntry{IPInfo: utils.NewIPInfo("not-an-ip")}, expected: false},
			},
		},
		{
			name: "Invalid CIDR in config",
			yamlContent: `
version: "1.0"
chains:
  - name: "CIDRTest"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          ip: "cidr:192.168.1.0/33"`,
			expectError: "invalid CIDR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runMatcherTest(t, tt.yamlContent, tt.testCases, tt.expectError)
		})
	}
}

func TestLoadConfigFromYAML_IntMatcherFallback(t *testing.T) {
	// This test specifically targets the non-StatusCode path in compileIntMatcher.
	// We use a field that is not an integer (Method) to ensure the logic
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
          method: 123 # Using an integer value on a string field
`
	tmpConfigPath := setupTestYAML(t, yamlContent)

	// --- Act ---
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() failed: %v", err)
	}

	// --- Assert ---
	if len(loadedCfg.Chains) != 1 || len(loadedCfg.Chains[0].Steps[0].Matchers) != 1 {
		t.Fatal("Failed to load or compile the int matcher fallback chain.")
	}

	matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

	// This entry should NOT match because "GET" != "123"
	if matcher.Matcher(&types.LogEntry{Method: "GET"}) {
		t.Error("Matcher incorrectly matched 'GET' with integer 123.")
	}
}

func runErrorTest(t *testing.T, name, yamlContent, expectedError string) {
	t.Run(name, func(t *testing.T) {
		tmpConfigPath := ""
		if name == "File Not Found" {
			// For this specific test, ensure the file does not exist.
			tmpConfigPath = filepath.Join(t.TempDir(), "nonexistent.yaml")
		} else {
			tmpConfigPath = setupTestYAML(t, yamlContent)
		}

		// For tests that expect non-fatal errors, we can suppress the log output
		// to keep the test runner output clean.
		if name == "File Matcher Not Found (Non-Fatal)" {
			originalLogFunc := logging.LogOutput
			logging.LogOutput = func(level logging.LogLevel, tag string, format string, args ...interface{}) {}
			t.Cleanup(func() { logging.LogOutput = originalLogFunc })
		}

		_, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})

		if expectedError == "" {
			if err != nil {
				t.Errorf("Expected no error, but got: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), expectedError) {
			t.Errorf("Expected error containing '%s', but got: %v", expectedError, err)
		}
	})
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
			expectedError: "failed to stat config file",
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
			expectedError: "YAML syntax error",
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
			name: "Invalid Regex",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "path": "regex:/(" } } ]
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
			name: "Object Matcher with Non-Integer Value",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "statuscode": { gte: "400" } } } ]
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
    steps: [ { field_matches: { "statuscode": { eq: 400 } } } ]
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
    steps: [ { field_matches: { "statuscode": {} } } ]
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
    steps: [ { field_matches: { "statuscode": true } } ]
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
    steps: [ { field_matches: { "path": ["/good", { "gt": 1 }] } } ]
`,
			expectedError: "chain 'Test', step 1: operator 'gt' is only supported for numeric fields, not 'Path'",
		},
		{
			name: "File Matcher Not Found (Non-Fatal)",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "path": "file:/path/to/nonexistent/file.txt" } } ]
`,
			expectedError: "", // Should NOT produce a fatal error
		},
		{
			name: "CIDR on non-IP field",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    steps: [ { field_matches: { "path": "cidr:192.168.1.0/24" } } ]
`,
			expectedError: "'cidr:' matcher is only supported for the 'IP' field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runErrorTest(t, tt.name, tt.yamlContent, tt.expectedError)
		})
	}
}

func TestLoadConfigFromYAML_Warnings(t *testing.T) {
	tests := []struct {
		name            string
		yamlContent     string
		expectedWarning string
	}{
		{
			name: "Block Action Missing Duration",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    match_key: "ip"
    action: "block" # No block_duration and no default
`,
			expectedWarning: "chain 'Test' has action 'block' but block_duration is missing or zero and no default is set. This chain will be skipped.",
		},
		{
			name: "Block Duration without any Duration Tables",
			yamlContent: `
version: "1.0"
chains:
  - name: "Test"
    action: "block"
    match_key: "ip"
    block_duration: "1w"
`,
			expectedWarning: "chain 'Test' has a block_duration of '1w', but no 'duration_tables' are configured",
		},
		{
			name: "Block Duration Not in Defined Duration Tables",
			yamlContent: `
version: "1.0"
blockers:
  backends:
    haproxy:
      duration_tables:
        "5m": "table_5m"
chains:
  - name: "Test"
    action: "block"
    match_key: "ip"
    block_duration: "10m" # This duration is not in the table
`,
			expectedWarning: "chain 'Test' has a block_duration of '10m' which is not defined in 'duration_tables'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runWarningTest(t, tt.yamlContent, tt.expectedWarning)
		})
	}
}

// runWarningTest is a helper to check for non-fatal warnings logged during config load.
func runWarningTest(t *testing.T, yamlContent, expectedWarning string) {
	t.Helper()
	tmpConfigPath := setupTestYAML(t, yamlContent)
	var capturedLogs []string
	var logMutex sync.Mutex
	originalLogFunc := logging.LogOutput
	logging.LogOutput = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		capturedLogs = append(capturedLogs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { logging.LogOutput = originalLogFunc })

	// Act
	_, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() returned an unexpected fatal error: %v", err)
	}

	// Assert
	logMutex.Lock()
	defer logMutex.Unlock()
	if !strings.Contains(strings.Join(capturedLogs, "\n"), expectedWarning) {
		t.Errorf("Expected log output to contain warning '%s', but it did not. Logs:\n%s", expectedWarning, strings.Join(capturedLogs, "\n"))
	}

}

func TestLoadConfigFromYAML_InvalidDurations(t *testing.T) {
	tests := []struct {
		name          string
		yamlContent   string
		expectedError string
	}{
		{
			name: "Invalid polling_interval",
			yamlContent: `
version: "1.0"
application:
  config:
    polling_interval: "5x"`,
			expectedError: "invalid polling_interval format",
		},
		{
			name: "Invalid cleanup_interval",
			yamlContent: `
version: "1.0"
checker:
  actor_cleanup_interval: "1y"`,
			expectedError: "invalid cleanup_interval format",
		},
		{
			name: "Invalid idle_timeout",
			yamlContent: `
version: "1.0"
checker:
  actor_state_idle_timeout: "30z"`,
			expectedError: "invalid idle_timeout format",
		},
		{
			name: "Invalid out_of_order_tolerance",
			yamlContent: `
version: "1.0"
parser:
  out_of_order_tolerance: "bad"`,
			expectedError: "invalid out_of_order_tolerance format",
		},
		{
			name: "Invalid eof_polling_delay",
			yamlContent: `
version: "1.0"
application:
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
blockers:
  backends:
    haproxy:
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
blockers:
  default_duration: "1x"
chains: []
`,
			expectedError: "invalid block_duration format for default_block_duration",
		},
		{
			name: "Invalid blocker_retry_delay",
			yamlContent: `
version: "1.0"
blockers:
  retry_delay: "bad"`,
			expectedError: "invalid blocker_retry_delay",
		},
		{
			name: "Invalid blocker_dial_timeout",
			yamlContent: `
version: "1.0"
blockers:
  dial_timeout: "1y"`,
			expectedError: "invalid blocker_dial_timeout",
		},
	}...)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runErrorTest(t, tt.name, tt.yamlContent, tt.expectedError)
		})
	}
}

func TestLoadConfigFromYAML_MissingOptionalCaptureGroup(t *testing.T) {
	// This test verifies that a custom regex is valid even if it's missing
	// optional capture groups like 'Referrer'.

	// Regex without the 'Referrer' named capture group.
	yamlContent := `
version: "1.0"
parser:
  log_format_regex: '^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{1,3}) \d+ \"[^\"]*\" \"(?P<UserAgent>[^\"]*)\"$'
chains: []
`
	tmpConfigPath := setupTestYAML(t, yamlContent)

	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() failed unexpectedly: %v", err)
	}

	if loadedCfg.LogFormatRegex == nil {
		t.Fatal("Expected LogFormatRegex to be loaded, but it was nil.")
	}

	p := testutil.NewTestProcessor(&config.AppConfig{Parser: config.ParserConfig{TimestampFormat: loadedCfg.Parser.TimestampFormat}}, nil)
	p.LogRegex = loadedCfg.LogFormatRegex

	logLine := `example.com 192.0.2.1 - - [01/Jan/2025:12:00:00 +0000] "GET /path HTTP/1.1" 200 123 "http://a.real.referrer" "TestAgent/1.0"`

	parsedEntry, err := parser.ParseLogLine(p, logLine)
	if err != nil {
		t.Fatalf("parser.ParseLogLine failed: %v", err)
	}
	entry := &types.LogEntry{
		Timestamp: parsedEntry.Timestamp,
		IPInfo:    utils.NewIPInfo(parsedEntry.IPInfo.Address),
		Method:    parsedEntry.Method,
		Path:      parsedEntry.Path,
		Protocol:  parsedEntry.Protocol,
		Referrer:  parsedEntry.Referrer,
		UserAgent: parsedEntry.UserAgent,
	}
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
parser:
  log_format_regex: '^(?P<VHost>\S+) \S+ \S+ \S+ \[(?P<Timestamp>[^\]]+)\] ".+"$'
chains: []
`
	tmpConfigPath := setupTestYAML(t, yamlContent)

	_, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
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
parser:
  timestamp_format: "%s" # Use RFC3339 for this test
  log_format_regex: '^(?P<IP>\S+) \[(?P<Timestamp>[^\]]+)\] (?P<Path>\S+)$'
chains: []
`, time.RFC3339)

	tmpConfigPath := setupTestYAML(t, yamlContent)

	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() failed unexpectedly: %v", err)
	}

	if loadedCfg.Parser.TimestampFormat != time.RFC3339 {
		t.Fatalf("Expected TimestampFormat to be loaded as RFC3339, but got: '%s'", loadedCfg.Parser.TimestampFormat)
	}

	// Now, test the parsing with this config.
	p := testutil.NewTestProcessor(&config.AppConfig{Parser: config.ParserConfig{TimestampFormat: loadedCfg.Parser.TimestampFormat}}, nil)
	p.LogRegex = loadedCfg.LogFormatRegex

	logLine := `192.0.2.1 [2025-01-01T12:00:00Z] /test`

	parsedEntry, err := parser.ParseLogLine(p, logLine)
	if err != nil {
		t.Fatalf("parser.ParseLogLine failed: %v", err)
	}
	entry := &types.LogEntry{
		Timestamp: parsedEntry.Timestamp,
		IPInfo:    utils.NewIPInfo(parsedEntry.IPInfo.Address),
		Method:    parsedEntry.Method,
		Path:      parsedEntry.Path,
		Protocol:  parsedEntry.Protocol,
		Referrer:  parsedEntry.Referrer,
	}
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

func TestConfigWatcher_Reload(t *testing.T) {
	// --- Setup ---
	// This test involves loading configs which can be noisy.
	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

	// 1. Create a temporary YAML file with initial content.
	initialYAMLContent := `
version: "1.0"
application:
  log_level: "info"

chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {path: "/initial"}}]
`
	tmpConfigPath := setupTestYAML(t, initialYAMLContent)

	// 2. Load the initial configuration.
	initialLoadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("Initial config.LoadConfigFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config.
	processor := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        initialLoadedCfg.Chains, // Set initial chains
		Config: &config.AppConfig{ // Set initial config state
			Application: config.ApplicationConfig{
				Config: config.ConfigManagement{
					PollingInterval: 10 * time.Millisecond,
				},
			},
			FileDependencies: initialLoadedCfg.FileDependencies,
		},
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
		TestSignals: &app.TestSignals{
			ForceCheckSignal: make(chan struct{}, 1),
			ReloadDoneSignal: make(chan struct{}, 1),
		},
		ConfigPath: tmpConfigPath,
	}
	// Set LastModTime to the actual modification time of the initial file.
	initialFileInfo, err := os.Stat(tmpConfigPath)
	if err != nil {
		t.Fatalf("Failed to stat initial temp yaml file: %v", err)
	}
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// 4. Start the app.ConfigWatcher with the test signal channel.
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go app.ConfigWatcher(processor, stopWatcher)

	// --- Act ---
	// 5. Modify the YAML file on disk.
	modifiedYAMLContent := `
version: "1.0"
application:
  log_level: "debug" # Changed log level

chains:
  - name: "ReloadedChain" # Changed chain name
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {path: "/reloaded"}}]
`
	if err := os.WriteFile(tmpConfigPath, []byte(modifiedYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write modified temp yaml file: %v", err)
	}
	// Manually advance the file's modification time to guarantee the watcher sees a change.
	// This is faster and more reliable than time.Sleep().
	futureTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(tmpConfigPath, futureTime, futureTime); err != nil {
		t.Fatalf("Failed to change file modification time: %v", err)
	}

	// 6. Force the watcher to check immediately.
	processor.TestSignals.ForceCheckSignal <- struct{}{}

	// 7. Wait for the reload signal from the watcher.
	select {
	case <-processor.TestSignals.ReloadDoneSignal:
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
          useragent: "file:%s"
`, agentFilePath)

	tmpConfigPath := setupTestYAML(t, initialYAMLContent)
	// 3. Load the initial configuration.
	initialLoadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("Initial config.LoadConfigFromYAML() failed: %v", err)
	}

	// 4. Create the processor with the initial config.
	processor := testutil.NewTestProcessor(&config.AppConfig{
		FileDependencies: initialLoadedCfg.FileDependencies,
		Application: config.ApplicationConfig{
			Config: config.ConfigManagement{
				PollingInterval: 10 * time.Millisecond,
			},
		},
	}, initialLoadedCfg.Chains)
	processor.ConfigPath = tmpConfigPath
	processor.TestSignals = &app.TestSignals{
		ForceCheckSignal: make(chan struct{}, 1),
		ReloadDoneSignal: make(chan struct{}, 1),
	}
	initialFileInfo, _ := os.Stat(tmpConfigPath)
	processor.Config.LastModTime = initialFileInfo.ModTime() // Set initial mod time

	// 5. Start the app.ConfigWatcher with the test signal channel.
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go app.ConfigWatcher(processor, stopWatcher)
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
	processor.TestSignals.ForceCheckSignal <- struct{}{}

	// 8. Wait for the reload signal from the watcher.
	select {
	case <-processor.TestSignals.ReloadDoneSignal:
		// Reload completed successfully.
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for configuration reload.")
	}

	// --- Assert ---
	// 9. Check if the processor's internal matchers have been updated.
	processor.ConfigMutex.RLock()
	defer processor.ConfigMutex.RUnlock()

	if len(processor.Chains) != 1 || len(processor.Chains[0].Steps) != 1 {
		t.Fatal("app.Processor chains were not reloaded correctly.")
	}

	// Create log entries to test the old and new rules.
	entryWithOldAgent := &types.LogEntry{UserAgent: "InitialBadAgent/1.0"}
	entryWithNewAgent := &types.LogEntry{UserAgent: "ReloadedBadAgent/2.0"}

	// The single matcher function should now only match the new agent.
	matcherFunc := processor.Chains[0].Steps[0].Matchers[0]
	if matcherFunc.Matcher(entryWithOldAgent) || !matcherFunc.Matcher(entryWithNewAgent) {
		t.Error("The file-based matcher was not updated correctly after the dependency file was reloaded.")
	}
}

func TestConfigWatcher_ReloadFailure(t *testing.T) {
	// --- Setup ---
	// This test involves loading configs which can be noisy.
	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

	initialYAMLContent := `
version: "1.0"
application:
  log_level: "info"
chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {path: "/initial"}}]
`
	tmpConfigPath := setupTestYAML(t, initialYAMLContent)
	// 2. Load the initial configuration.
	initialLoadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("Initial config.LoadConfigFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config and a log capturer.
	var capturedLogs []string
	var logMutex sync.Mutex
	processor := &app.Processor{
		ConfigMutex: &sync.RWMutex{},
		Chains:      initialLoadedCfg.Chains, // This is fine, as it's a local type
		Config:      &config.AppConfig{Application: config.ApplicationConfig{Config: config.ConfigManagement{}}},
		TestSignals: &app.TestSignals{
			ForceCheckSignal: make(chan struct{}, 1),
			ReloadDoneSignal: make(chan struct{}, 1),
		},
		ConfigPath: tmpConfigPath,
	}
	processor.Config.Application.Config.PollingInterval = 10 * time.Millisecond
	initialFileInfo, _ := os.Stat(tmpConfigPath)
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

	// 5. Start the app.ConfigWatcher.
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go app.ConfigWatcher(processor, stopWatcher)

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
    steps: [{field_matches: {path: "regex:("}}]
`
	if err := os.WriteFile(tmpConfigPath, []byte(invalidYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write invalid YAML: %v", err)
	}
	// Manually advance the file's modification time.
	futureTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(tmpConfigPath, futureTime, futureTime); err != nil {
		t.Fatalf("Failed to change file modification time: %v", err)
	}

	// 7. Force the watcher to perform a check immediately, bypassing the timer.
	processor.TestSignals.ForceCheckSignal <- struct{}{}

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
    steps: [{field_matches: {path: "/initial"}}]
`
	tmpConfigPath := setupTestYAML(t, initialYAMLContent)

	// 2. Create the processor with a log capturer.
	var capturedLogs []string
	var logMutex sync.Mutex
	processor := &app.Processor{
		ConfigMutex: &sync.RWMutex{},
		Chains:      []config.BehavioralChain{{Name: "InitialChain"}}, // Simplified initial state
		Config:      &config.AppConfig{Application: config.ApplicationConfig{Config: config.ConfigManagement{}}},
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {
			logMutex.Lock()
			capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
			logMutex.Unlock()
		},
		TestSignals: &app.TestSignals{
			ForceCheckSignal: make(chan struct{}, 1),
		},
		ConfigPath: tmpConfigPath,
	}
	processor.Config.Application.Config.PollingInterval = 10 * time.Millisecond
	initialFileInfo, _ := os.Stat(tmpConfigPath)
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
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go app.ConfigWatcher(processor, stopWatcher)

	// --- Act ---
	// 4. Delete the YAML file to trigger a stat error on the next poll.
	// We add a small sleep to ensure the watcher has started and is ready.
	time.Sleep(50 * time.Millisecond)
	if err := os.Remove(tmpConfigPath); err != nil {
		t.Fatalf("Failed to remove temp file: %v", err)
	}

	// 5. Force the watcher to check immediately.
	processor.TestSignals.ForceCheckSignal <- struct{}{}

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

func TestStart_WatcherSelection(t *testing.T) {
	tests := []struct {
		name                  string
		reloadOnFlag          string
		expectWatcherLog      string
		dontExpectReloaderLog bool
	}{
		{
			name:                  "Default behavior starts both watcher and SIGHUP reloader",
			reloadOnFlag:          "", // Default case
			expectWatcherLog:      "Signal-based config reloading enabled. Send HUP signal to reload.",
			dontExpectReloaderLog: false,
		},
		{
			name:                  "HUP starts app.SignalReloader only",
			reloadOnFlag:          "HUP",
			expectWatcherLog:      "Signal-based config reloading enabled. Send HUP signal to reload.",
			dontExpectReloaderLog: false,
		},
		{
			name:                  "watcher starts app.ConfigWatcher only",
			reloadOnFlag:          "watcher",
			expectWatcherLog:      "Starting app.ConfigWatcher, polling every", // Partial match due to dynamic polling interval
			dontExpectReloaderLog: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Setup ---
			var capturedLogs []string
			var logMutex sync.Mutex
			logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
				logMutex.Lock()
				defer logMutex.Unlock()
				capturedLogs = append(capturedLogs, fmt.Sprintf(tag+": "+format, args...))
			}

			p := testutil.NewTestProcessor(&config.AppConfig{}, nil)
			p.ReloadOn = tt.reloadOnFlag
			p.LogFunc = logFunc

			stopCh := make(chan struct{})
			defer close(stopCh)

			// --- Act ---
			// This is a simplified, test-safe version of the logic in main.start()
			if strings.ToLower(p.ReloadOn) != "watcher" {
				go app.SignalReloader(p, stopCh, make(chan os.Signal, 1))
			} else {
				go app.ConfigWatcher(p, stopCh)
			}

			time.Sleep(20 * time.Millisecond) // Give the goroutine a moment to start and log.

			// --- Assert ---
			logMutex.Lock()
			defer logMutex.Unlock()
			logOutput := strings.Join(capturedLogs, "\n")

			if !strings.Contains(logOutput, tt.expectWatcherLog) {
				t.Errorf("Expected log output to contain '%s', but it did not. Logs:\n%s", tt.expectWatcherLog, logOutput)
			}

			if tt.dontExpectReloaderLog && strings.Contains(logOutput, "Signal-based config reloading") {
				t.Errorf("Expected app.SignalReloader not to start, but its log was found. Logs:\n%s", logOutput)
			}
		})
	}
}

// runMatcherTest is a helper to reduce boilerplate in matcher tests.
func runMatcherTest(t *testing.T, yamlContent string, testCases map[string]struct {
	entry    *types.LogEntry
	expected bool
}, expectError string) {
	t.Helper()
	tmpConfigPath := setupTestYAML(t, yamlContent)
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if expectError != "" {
		if err == nil || !strings.Contains(err.Error(), expectError) {
			t.Fatalf("Expected error containing '%s', but got: %v", expectError, err)
		}
		return // Error was expected and occurred.
	}
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() failed: %v", err)
	}
	matcher := loadedCfg.Chains[0].Steps[0].Matchers[0]

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var val interface{}
			if tc.entry != nil {
				val = tc.entry.Path
				if tc.entry.IPInfo.Address != "" {
					val = tc.entry.IPInfo.Address
				}
				if tc.entry.StatusCode != 0 {
					val = tc.entry.StatusCode
				}
				if tc.entry.Size != 0 {
					val = tc.entry.Size
				}
			}
			if got := matcher.Matcher(tc.entry); got != tc.expected {
				// Use a more generic error message to handle both Path and StatusCode tests.
				t.Errorf("Matcher returned %v, expected %v for entry value '%v'", got, tc.expected, val)
			}
		})
	}
}

func TestLoadConfigFromYAML_GoodActors(t *testing.T) {
	// --- Setup ---
	// Create a temporary file for the file-based matcher.
	tempDir := t.TempDir()
	ipListPath := filepath.Join(tempDir, "google_ips.txt")
	if err := os.WriteFile(ipListPath, []byte("cidr:8.8.8.0/24"), 0644); err != nil {
		t.Fatalf("Failed to write temp ip list file: %v", err)
	}

	yamlContent := fmt.Sprintf(`
version: "1.0"
good_actors:
  - name: "our_network"
    ip:
      - "cidr:10.10.10.0/24"
      - "127.0.0.1"
  - name: "googlebot"
    ip: "file:%s"
    useragent: "regex:(?i)googlebot"
  - name: "free_agent"
    useragent: "neverblocked"
`, ipListPath)

	tmpConfigPath := setupTestYAML(t, yamlContent)
	t.Cleanup(testutil.ResetGlobalState)

	// --- Act ---
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("config.LoadConfigFromYAML() returned an unexpected error: %v", err)
	}

	// --- Assert ---
	if len(loadedCfg.GoodActors) != 3 {
		t.Fatalf("Expected 3 good actor definitions to be loaded, got %d", len(loadedCfg.GoodActors))
	}

	// Find the 'googlebot' definition for detailed testing
	var googlebotDef config.GoodActorDef
	for _, def := range loadedCfg.GoodActors {
		if def.Name == "googlebot" {
			googlebotDef = def
			break
		}
	}

	if googlebotDef.Name == "" {
		t.Fatal("Could not find 'googlebot' good actor definition.")
	}

	// Test the compiled matchers
	googleIPEntry := &types.LogEntry{IPInfo: utils.NewIPInfo("8.8.8.8"), UserAgent: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"}

	// The IP matcher should match
	if len(googlebotDef.IPMatchers) == 0 || !googlebotDef.IPMatchers[0](googleIPEntry) {
		t.Error("Googlebot IP matcher failed for a matching IP.")
	}
	// The UserAgent matcher should match
	if len(googlebotDef.UAMatchers) == 0 || !googlebotDef.UAMatchers[0](googleIPEntry) {
		t.Error("Googlebot UserAgent matcher failed for a matching UserAgent.")
	}
}

// TestFindNewlyAddedGoodActors verifies that the helper correctly identifies newly added good actors.
func TestConfigReload_UnblockGoodActors(t *testing.T) {
	testutil.ResetGlobalState()
	t.Cleanup(testutil.ResetGlobalState)

	// Setup: Create initial config without good actors
	initialYAML := `
version: "1.0"
application:
  log_level: "info"
checker:
  unblock_on_good_actor: true

chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {path: "/"}}]
`
	tmpConfigPath := setupTestYAML(t, initialYAML)

	// Load initial config
	initialLoadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tmpConfigPath})
	if err != nil {
		t.Fatalf("Initial config.LoadConfigFromYAML() failed: %v", err)
	}

	// Track unblock calls
	unblockCalled := make(map[string]bool)
	unblockMutex := &sync.Mutex{}

	// Create processor with some blocked IPs
	processor := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        initialLoadedCfg.Chains,
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				Config: config.ConfigManagement{
					PollingInterval: 10 * time.Millisecond,
				},
			},
			FileDependencies: initialLoadedCfg.FileDependencies,
			GoodActors:       initialLoadedCfg.GoodActors,
			Checker: config.CheckerConfig{
				UnblockOnGoodActor: true,
			},
		},
		DryRun:       false,
		ActiveBlocks: make(map[string]persistence.ActiveBlockInfo),
		Blocker: &testutil.MockBlocker{
			UnblockFunc: func(ipInfo utils.IPInfo, reason string) error {
				unblockMutex.Lock()
				unblockCalled[ipInfo.Address] = true
				unblockMutex.Unlock()
				return nil
			},
		},
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {
			// Silent for test
		},
		TestSignals: &app.TestSignals{
			ForceCheckSignal: make(chan struct{}, 1),
			ReloadDoneSignal: make(chan struct{}, 1),
		},
		ConfigPath: tmpConfigPath,
	}

	// Set LastModTime
	initialFileInfo, err := os.Stat(tmpConfigPath)
	if err != nil {
		t.Fatalf("Failed to stat initial temp yaml file: %v", err)
	}
	processor.Config.LastModTime = initialFileInfo.ModTime()

	// Add some blocked IPs
	processor.ActiveBlocks["1.2.3.4"] = persistence.ActiveBlockInfo{
		Reason:      "test-chain",
		UnblockTime: time.Now().Add(1 * time.Hour),
	}
	processor.ActiveBlocks["5.6.7.8"] = persistence.ActiveBlockInfo{
		Reason:      "test-chain",
		UnblockTime: time.Now().Add(1 * time.Hour),
	}

	// Start app.ConfigWatcher
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go app.ConfigWatcher(processor, stopWatcher)

	// Act: Modify config to add a good actor for one of the blocked IPs
	modifiedYAML := `
version: "1.0"
application:
  log_level: "info"
checker:
  unblock_on_good_actor: true

good_actors:
  - name: "whitelisted_ip"
    ip: "1.2.3.4"

chains:
  - name: "TestChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {path: "/"}}]
`
	if err := os.WriteFile(tmpConfigPath, []byte(modifiedYAML), 0644); err != nil {
		t.Fatalf("Failed to write modified temp yaml file: %v", err)
	}

	// Advance modification time
	futureTime := time.Now().Add(1 * time.Second)
	if err := os.Chtimes(tmpConfigPath, futureTime, futureTime); err != nil {
		t.Fatalf("Failed to change file modification time: %v", err)
	}

	// Force watcher to check
	processor.TestSignals.ForceCheckSignal <- struct{}{}

	// Wait for reload
	select {
	case <-processor.TestSignals.ReloadDoneSignal:
		// Reload completed
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for configuration reload.")
	}

	// Assert: Check that 1.2.3.4 was unblocked
	unblockMutex.Lock()
	if !unblockCalled["1.2.3.4"] {
		t.Error("Expected 1.2.3.4 to be unblocked after adding to good_actors")
	}
	if unblockCalled["5.6.7.8"] {
		t.Error("Expected 5.6.7.8 to remain blocked (not in good_actors)")
	}
	unblockMutex.Unlock()

	// Check activeBlocks
	processor.PersistenceMutex.Lock()
	if _, exists := processor.ActiveBlocks["1.2.3.4"]; exists {
		t.Error("Expected 1.2.3.4 to be removed from activeBlocks")
	}
	if _, exists := processor.ActiveBlocks["5.6.7.8"]; !exists {
		t.Error("Expected 5.6.7.8 to remain in activeBlocks")
	}
	processor.PersistenceMutex.Unlock()
}
