package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Mocking Setup for Blocker Interface ---

// MockBlocker implements the Blocker interface for testing, allowing Block() calls to be intercepted.
type MockBlocker struct {
	BlockFunc   func(ipInfo IPInfo, duration time.Duration) error
	UnblockFunc func(ipInfo IPInfo) error
}

// Block calls the stored mock function to simulate the blocking action.
func (m *MockBlocker) Block(ipInfo IPInfo, duration time.Duration) error {
	if m.BlockFunc != nil {
		return m.BlockFunc(ipInfo, duration)
	}
	return nil
}

// Unblock calls the stored mock function to simulate the unblocking action.
func (m *MockBlocker) Unblock(ipInfo IPInfo) error {
	if m.UnblockFunc != nil {
		return m.UnblockFunc(ipInfo)
	}
	return nil
}

// --- Test Cases for ParseLogLine ---
// (No changes to this section, as it passed in the previous step)

func TestParseLogLine(t *testing.T) {
	testTime, _ := time.Parse(AccessLogTimeFormat, "06/Nov/2025:09:00:00 +0100")

	validReferrer := "http://referrer.com/test?q=1"
	validUserAgent := "test-agent"
	validIPv6UserAgent := "test-agent-ipv6"

	// Correct format: VHost IP - userx [time] "request" status size "referrer" "user-agent"
	validLogLine := fmt.Sprintf(`www.example.com 192.168.1.1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path/to/resource HTTP/1.1" 200 1234 "%s" "%s"`, validReferrer, validUserAgent)
	validIPv6LogLine := `www.example.com 2001:db8::1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path/to/resource HTTP/1.1" 200 1234 "` + validReferrer + `" "` + validIPv6UserAgent + `"`

	tests := []struct {
		name        string
		line        string
		expectError bool
		expected    *LogEntry
	}{
		{
			name:        "Valid IPv4 Line",
			line:        validLogLine,
			expectError: false,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     NewIPInfo("192.168.1.1"),
				Path:       "/path/to/resource",
				Method:     "GET",
				Protocol:   "HTTP/1.1",
				UserAgent:  validUserAgent,
				Referrer:   validReferrer,
				StatusCode: 200,
			},
		},
		{
			name:        "Valid IPv6 Line",
			line:        validIPv6LogLine,
			expectError: false,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     NewIPInfo("2001:db8::1"),
				Path:       "/path/to/resource",
				Method:     "GET",
				Protocol:   "HTTP/1.1",
				UserAgent:  validIPv6UserAgent,
				Referrer:   validReferrer,
				StatusCode: 200,
			},
		},
		{
			name:        "Valid Line with Empty Referrer and UserAgent",
			line:        `www.example.com 192.168.1.3 - userx [06/Nov/2025:09:00:00 +0100] "PUT /upload HTTP/2.0" 201 500 "-" "-"`,
			expectError: false,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     NewIPInfo("192.168.1.3"),
				Path:       "/upload",
				Method:     "PUT",
				Protocol:   "HTTP/2.0",
				UserAgent:  "-",
				Referrer:   "-",
				StatusCode: 201,
			},
		},
		{
			name:        "Valid Line with Zero Status Code",
			line:        `www.example.com 192.168.1.4 - userx [06/Nov/2025:09:00:00 +0100] "GET /aborted HTTP/1.1" 0 0 "-" "-"`,
			expectError: false,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     NewIPInfo("192.168.1.4"),
				Path:       "/aborted",
				Method:     "GET",
				Protocol:   "HTTP/1.1",
				UserAgent:  "-",
				Referrer:   "-",
				StatusCode: 0,
			},
		},
		{
			name:        "Empty Line",
			line:        "",
			expectError: false,
			expected:    nil,
		},
		{
			name:        "Comment Line",
			line:        "# This is a comment",
			expectError: false,
			expected:    nil,
		},
		{
			name:        "Malformed Timestamp",
			line:        `www.example.com 192.168.1.1 - userx [06/Mal/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-" "-"`,
			expectError: true,
			expected:    nil,
		},
		{
			name:        "Malformed Status Code",
			line:        `www.example.com 192.168.1.1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path HTTP/1.1" XXX 1234 "-" "-"`,
			expectError: true,
			expected:    nil,
		},
		{
			name:        "Incorrect Field Count",
			line:        `www.example.com 192.168.1.1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-"`, // Missing UserAgent quote field
			expectError: true,
			expected:    nil,
		},
		{
			name:        "Invalid IP Address (Invalid Version)",
			line:        `www.example.com invalid-ip - userx [06/Nov/2025:09:00:00 +0100] "GET /path/to/resource HTTP/1.1" 200 1234 "-" "-"`,
			expectError: true,
			expected:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := ParseLogLine(tt.line)

			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected an error for line:\n%s\nGot nil.", tt.line)
				}
			} else {
				if err != nil {
					t.Fatalf("Unexpected error for line:\n%s\nError: %v", tt.line, err)
				}
				if !reflect.DeepEqual(entry, tt.expected) {
					t.Errorf("LogEntry mismatch.\nGot:  %+v\nWant: %+v", entry, tt.expected)
				}
			}
		})
	}
}

// TestProcessLogLine_DryRun checks that the Block() function is never called in DryRun mode,
// but the processing (including chain progression) still occurs in the dry-run activity store.
func TestProcessLogLine_DryRun(t *testing.T) {
	// Cleanup: Reset stores to ensure test isolation
	t.Cleanup(resetGlobalState)

	// Mock Blocker that will fail the test if called.
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			t.Fatal("Blocker.Block was called in DryRun mode.")
			return nil
		},
	}

	// Setup a simple chain that will match and call 'block'
	matcher, _ := compileStringMatcher("dryrun_chain", 0, "Path", "/1", new([]string))
	chain := BehavioralChain{
		Name: "dryrun_chain",
		Steps: []StepDef{
			{Order: 1, Matchers: []fieldMatcher{matcher}},
		},
		Action:        "block",
		BlockDuration: time.Minute,
		MatchKey:      "ip",
	}

	p := Processor{
		ActivityMutex:     &sync.RWMutex{},
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		Blocker:           mockBlocker,
		ChainMutex:        &sync.RWMutex{},
		Chains:            []BehavioralChain{chain},
		Config:            &AppConfig{},
		DryRun:            true, // CRITICAL: DryRun is enabled
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
		CheckChainsFunc:   func(entry *LogEntry) {}, // Will be replaced below
	}
	p.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(&p, line, lineNumber) }

	ip := "192.0.2.1"
	logLine := fmt.Sprintf(`www.example.com %s - userx [06/Nov/2025:09:00:00 +0100] "GET /1 HTTP/1.1" 200 1234 "-" "-"`, ip)
	key := TrackingKey{IPInfo: NewIPInfo(ip)}

	// Set the CheckChainsFunc to the real method on the processor instance.
	p.CheckChainsFunc = p.CheckChains

	// 1. Process the line
	p.ProcessLogLine(logLine, 1)

	// Assertion 1: Check the DryRun store. The activity should exist and be blocked.
	p.ActivityMutex.RLock()
	dryRunActivity, exists := p.ActivityStore[key]
	p.ActivityMutex.RUnlock()

	if !exists {
		t.Fatal("Expected activity in DryRun store, but none was found.")
	}

	if !dryRunActivity.IsBlocked {
		t.Error("Expected IP to be marked as blocked in DryRun store, but it was not.")
	}

	if dryRunActivity.BlockedUntil.IsZero() {
		t.Error("Expected BlockedUntil time to be set in DryRun store, but it was zero.")
	}
}

// TestProcessLogLineInternal_ParseError verifies that when processLogLineInternal
// receives a line that cannot be parsed, it logs the error and does not proceed.
func TestProcessLogLineInternal_ParseError(t *testing.T) {
	resetGlobalState()

	var logMutex sync.Mutex
	var capturedLog string
	logCaptureFunc := func(level LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		if tag == "PARSE_FAIL" {
			capturedLog = fmt.Sprintf(format, args...)
		}
	}

	p := &Processor{
		LogFunc: logCaptureFunc,
		// CheckChainsFunc should not be called if parsing fails.
		CheckChainsFunc: func(entry *LogEntry) {
			t.Error("CheckChains was called, but should have been skipped due to a parse error.")
		},
	}

	// Act: Process a malformed log line.
	malformedLine := "this is not a valid log line"
	processLogLineInternal(p, malformedLine, 123)

	if !strings.Contains(capturedLog, "Parsing failed") {
		t.Errorf("Expected a 'PARSE_FAIL' log message, but it was not captured. Got: '%s'", capturedLog)
	}
}

// TestGetMatchValue_UnknownField tests that trying to extract a value for a field not in LogEntry
// returns an error, ensuring validation on match fields.
func TestGetMatchValue_UnknownField(t *testing.T) {
	entry := &LogEntry{}

	_, err := GetMatchValue("UnknownField", entry)

	if err == nil {
		t.Fatal("Expected an error for unknown field, but got nil")
	}

	expectedErrMsg := "unknown field: UnknownField"
	if err.Error() != expectedErrMsg {
		t.Errorf("Error mismatch. Expected error '%s', got: %v", expectedErrMsg, err)
	}
}

func TestGetMatchValue_Success(t *testing.T) {
	entry := &LogEntry{
		IPInfo:     NewIPInfo("192.0.2.1"),
		Path:       "/test/path",
		Method:     "GET",
		Protocol:   "HTTP/1.1",
		UserAgent:  "TestAgent",
		Referrer:   "http://example.com",
		StatusCode: 404,
	}

	testCases := []struct {
		fieldName string
		expected  string
	}{
		{"IP", "192.0.2.1"},
		{"Path", "/test/path"},
		{"Method", "GET"},
		{"Protocol", "HTTP/1.1"},
		{"UserAgent", "TestAgent"},
		{"Referrer", "http://example.com"},
		{"StatusCode", "404"},
	}

	for _, tc := range testCases {
		t.Run(tc.fieldName, func(t *testing.T) {
			value, err := GetMatchValue(tc.fieldName, entry)
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			if value != tc.expected {
				t.Errorf("Expected value '%s', but got '%s'", tc.expected, value)
			}
		})
	}
}
