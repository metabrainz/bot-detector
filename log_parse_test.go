package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

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
				Size:       1234,
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
				Size:       1234,
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
				Size:       500,
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
				Size:       0,
			},
		},
		{
			name:        "Valid Line with Dash for Size",
			line:        `www.example.com 192.168.1.5 - userx [06/Nov/2025:09:00:00 +0100] "GET /no-content HTTP/1.1" 204 - "-" "-"`,
			expectError: false,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     NewIPInfo("192.168.1.5"),
				Path:       "/no-content",
				Method:     "GET",
				Protocol:   "HTTP/1.1",
				UserAgent:  "-",
				Referrer:   "-",
				StatusCode: 204,
				Size:       -1, // Should be parsed as -1
			},
		},
		{
			name:        "Malformed Request Field",
			line:        `www.example.com 192.168.1.6 - userx [06/Nov/2025:09:00:00 +0100] - 400 172 "-" "-"`,
			expectError: false,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     NewIPInfo("192.168.1.6"),
				Path:       "", // Should be empty
				Method:     "",
				Protocol:   "", // Should be empty
				UserAgent:  "-",
				Referrer:   "-",
				StatusCode: 400,
				Size:       172,
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
			// Create a processor to call the method on.
			p := &Processor{
				Config: &AppConfig{TimestampFormat: AccessLogTimeFormat},
			}
			entry, err := ParseLogLine(p, tt.line)

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

// TestParseLogLine_CustomRegex verifies that a custom log format regex provided
// in the processor's configuration is used instead of the default.
func TestParseLogLine_CustomRegex(t *testing.T) {
	// 1. Define a custom regex (e.g., standard combined log format without a VHost prefix).
	customRegexString := `^(?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{1,3}) (?P<Size>\d+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`
	customRegex, err := regexp.Compile(customRegexString)
	if err != nil {
		t.Fatalf("Failed to compile custom regex: %v", err)
	}

	// 2. Create a log line that matches the custom format but NOT the default format.
	customLogLine := `198.51.100.5 - - [10/Nov/2025:13:55:36 +0000] "GET /custom/path HTTP/1.1" 404 500 "http://custom.referrer/from" "CustomAgent/1.0"`

	// 3. Create a processor and assign the custom regex.
	p := &Processor{
		LogRegex: customRegex,
		Config:   &AppConfig{TimestampFormat: AccessLogTimeFormat},
	}

	// 4. Act: Parse the log line using the processor with the custom regex.
	entry, err := ParseLogLine(p, customLogLine)

	// 5. Assert: The parsing should succeed and the data should be correct.
	if err != nil {
		t.Fatalf("ParseLogLine with custom regex failed unexpectedly: %v", err)
	}

	expectedTime, _ := time.Parse(AccessLogTimeFormat, "10/Nov/2025:13:55:36 +0000")
	expectedEntry := &LogEntry{
		Timestamp:  expectedTime,
		IPInfo:     NewIPInfo("198.51.100.5"),
		Method:     "GET",
		Path:       "/custom/path",
		Protocol:   "HTTP/1.1",
		Referrer:   "http://custom.referrer/from",
		StatusCode: 404,
		UserAgent:  "CustomAgent/1.0",
		Size:       500,
	}

	if !reflect.DeepEqual(entry, expectedEntry) {
		t.Errorf("LogEntry mismatch.\nGot:  %+v\nWant: %+v", entry, expectedEntry)
	}

	// 6. Control Assertion: Verify the default parser (processor with nil regex) FAILS.
	defaultProcessor := &Processor{
		Config: &AppConfig{TimestampFormat: AccessLogTimeFormat},
	}
	_, defaultErr := ParseLogLine(defaultProcessor, customLogLine)
	if defaultErr == nil {
		t.Error("Expected the default parser to fail on the custom log line, but it succeeded.")
	}
	expectedErrorString := "line does not match log format regex"
	if !strings.Contains(defaultErr.Error(), expectedErrorString) {
		t.Errorf("Expected error to contain '%s', but got: %v", expectedErrorString, defaultErr)
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

	p := newTestProcessor(&AppConfig{
		TimestampFormat: AccessLogTimeFormat, // Set the required timestamp format
	}, []BehavioralChain{chain})
	p.DryRun = true
	p.Blocker = mockBlocker

	p.ProcessLogLine = func(line string) { processLogLineInternal(p, line) }

	ip := "192.0.2.1"
	logLine := fmt.Sprintf(`www.example.com %s - userx [06/Nov/2025:09:00:00 +0100] "GET /1 HTTP/1.1" 200 1234 "-" "-"`, ip)
	key := TrackingKey{IPInfo: NewIPInfo(ip)}

	// Set the CheckChainsFunc to the real method on the processor instance.
	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) }

	// 1. Process the line
	p.ProcessLogLine(logLine)

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
	var capturedMessage string
	var capturedLevel logging.LogLevel
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		if tag == "PARSE_FAIL" {
			capturedMessage = fmt.Sprintf(format, args...)
			capturedLevel = level
		}
	}

	p := newTestProcessor(nil, nil)
	p.LogFunc = logCaptureFunc
	p.CheckChainsFunc = func(entry *LogEntry) {
		t.Error("CheckChains was called, but should have been skipped due to a parse error.")
	}

	// Act: Process a malformed log line.
	malformedLine := "this is not a valid log line"
	processLogLineInternal(p, malformedLine)

	if !strings.Contains(capturedMessage, "Parsing failed") {
		t.Errorf("Expected a 'PARSE_FAIL' log message, but it was not captured. Got: '%s'", capturedMessage)
	}

	if capturedLevel != logging.LevelDebug {
		t.Errorf("Expected log level for parse failure during testing to be LevelDebug, but got %v", capturedLevel)
	}
}

// TestProcessLogLineInternal_SkipLine verifies that a comment or empty line is skipped
// and the appropriate log message is generated.
func TestProcessLogLineInternal_SkipLine(t *testing.T) {
	resetGlobalState()

	var logMutex sync.Mutex
	var capturedMessage string
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		if tag == "SKIP" {
			capturedMessage = fmt.Sprintf(format, args...)
		}
	}

	p := newTestProcessor(nil, nil)
	p.LogFunc = logCaptureFunc
	p.CheckChainsFunc = func(entry *LogEntry) {
		t.Error("CheckChains was called, but should have been skipped for a comment line.")
	}

	processLogLineInternal(p, "# this is a comment")
	if !strings.Contains(capturedMessage, "Skipped empty/comment line.") {
		t.Errorf("Expected a 'SKIP' log message, but it was not captured. Got: '%s'", capturedMessage)
	}
}

// TestGetMatchValue_UnknownField tests that trying to extract a value for a field not in LogEntry
// returns an error, ensuring validation on match fields.
func TestGetMatchValue_UnknownField(t *testing.T) {
	entry := &LogEntry{}

	_, _, err := GetMatchValue("UnknownField", entry)

	if err == nil {
		t.Fatal("Expected an error for unknown field, but got nil")
	}

	expectedErrMsg := "unknown field: 'UnknownField'"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch. Expected error containing '%s', but got: %v", expectedErrMsg, err)
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
		fieldName    string
		expectedVal  interface{}
		expectedType FieldType
	}{
		{"IP", "192.0.2.1", StringField},
		{"Path", "/test/path", StringField},
		{"Method", "GET", StringField},
		{"Protocol", "HTTP/1.1", StringField},
		{"UserAgent", "TestAgent", StringField},
		{"Referrer", "http://example.com", StringField},
		{"StatusCode", 404, IntField},
	}

	for _, tc := range testCases {
		t.Run(tc.fieldName, func(t *testing.T) {
			value, fieldType, err := GetMatchValue(tc.fieldName, entry)
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			if value != tc.expectedVal {
				t.Errorf("Expected value '%v' (%T), but got '%v' (%T)", tc.expectedVal, tc.expectedVal, value, value)
			}
			if fieldType != tc.expectedType {
				t.Errorf("Expected field type %v, but got %v", tc.expectedType, fieldType)
			}
		})
	}
}

func TestGetMatchValueIfType(t *testing.T) {
	entry := &LogEntry{
		IPInfo:     NewIPInfo("192.0.2.1"),
		Path:       "/test/path",
		StatusCode: 404,
	}

	testCases := []struct {
		name          string
		fieldName     string
		expectedType  FieldType
		expectedValue interface{}
	}{
		{
			name:          "Correct Type - String",
			fieldName:     "Path",
			expectedType:  StringField,
			expectedValue: "/test/path",
		},
		{
			name:          "Correct Type - Int",
			fieldName:     "StatusCode",
			expectedType:  IntField,
			expectedValue: 404,
		},
		{
			name:          "Incorrect Type - Expect String, Got Int",
			fieldName:     "StatusCode",
			expectedType:  StringField,
			expectedValue: nil,
		},
		{
			name:          "Incorrect Type - Expect Int, Got String",
			fieldName:     "Path",
			expectedType:  IntField,
			expectedValue: nil,
		},
		{
			name:          "Unknown Field",
			fieldName:     "UnknownField",
			expectedType:  StringField,
			expectedValue: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			value := GetMatchValueIfType(tc.fieldName, entry, tc.expectedType)

			if value != tc.expectedValue {
				t.Errorf("Expected value '%v' (%T), but got '%v' (%T)", tc.expectedValue, tc.expectedValue, value, value)
			}
		})
	}
}
