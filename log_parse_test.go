package main

import (
	"fmt"
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
	validIPv6LogLine := fmt.Sprintf(`www.example.com 2001:db8::1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path/to/resource HTTP/1.1" 200 1234 "%s" "%s"`, validReferrer, validIPv6UserAgent)

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
			line:        fmt.Sprintf(`www.example.com 192.168.1.1 - userx [06/Mal/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-" "-"`),
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
				if entry == nil {
					if tt.expected != nil {
						t.Fatalf("Expected entry %v, got nil.", tt.expected)
					}
					return
				}

				if entry.IPInfo != tt.expected.IPInfo {
					t.Errorf("IPInfo mismatch. Expected %+v, got %+v", tt.expected.IPInfo, entry.IPInfo)
				}
				if entry.Path != tt.expected.Path {
					t.Errorf("Path mismatch. Expected %s, got %s", tt.expected.Path, entry.Path)
				}
				if entry.UserAgent != tt.expected.UserAgent {
					t.Errorf("UserAgent mismatch. Expected %s, got %s", tt.expected.UserAgent, entry.UserAgent)
				}
			}
		})
	}
}

// --- Test Cases for ProcessLogLine Flow Control ---

func TestProcessLogLine_FlowControl(t *testing.T) {
	// Cleanup: Ensure the global store is reset after the test.
	t.Cleanup(resetGlobalState)

	var blockCount int32
	var blockMu sync.Mutex

	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockMu.Lock()
			blockCount++
			blockMu.Unlock()
			return nil
		},
	}

	// Base LogEntry info to construct log lines
	baseIP := "192.0.2.1"
	whitelistedIP := "192.0.2.2"
	invalidIP := "invalid-ip" // DECLARED AND NOW USED

	// Base Processor
	p := Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        nil,
		ChainMutex:    &sync.RWMutex{},
		Blocker:       mockBlocker,
		LogFunc:       LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool {
			return ipInfo.Address == whitelistedIP // Only this IP is whitelisted
		},
		Config: &AppConfig{},
		DryRun: false,
	}
	// Assign the real implementation to the function field to avoid a nil pointer panic.
	// The test will call this function.
	p.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(&p, line, lineNumber) }

	// Helper function to create a valid log line
	createLine := func(ip string) string {
		return fmt.Sprintf(`www.example.com %s - userx [06/Nov/2025:09:00:00 +0100] "GET /path/to/resource HTTP/1.1" 200 1234 "-" "-"`, ip)
	}
	// Helper function to create a log line with a specific timestamp.
	createLineWithTimestamp := func(ip string, ts time.Time) string {
		tsFormatted := ts.Format("02/Jan/2006:15:04:05 -0700")
		return fmt.Sprintf(`www.example.com %s - userx [%s] "GET /path/to/resource HTTP/1.1" 200 1234 "-" "-"`, ip, tsFormatted)
	}

	tests := []struct {
		name             string
		line             string
		setup            func(p *Processor) // Setup function to configure store state before processing
		assertBlockCount int32
		assertIsBlocked  bool
	}{
		{
			name:             "Skip - Comment Line",
			line:             "# This is a comment",
			setup:            func(p *Processor) {},
			assertBlockCount: 0,
			assertIsBlocked:  false,
		},
		{
			name:             "Skip - Invalid IP Line (Parse Fail)",
			line:             createLine(invalidIP), // FIXED: Now using the invalidIP variable
			setup:            func(p *Processor) {},
			assertBlockCount: 0,
			assertIsBlocked:  false,
		},
		{
			name:             "Skip - Whitelisted IP",
			line:             createLine(whitelistedIP),
			setup:            func(p *Processor) {},
			assertBlockCount: 0,
			assertIsBlocked:  false,
		},
		{
			name: "Skip - Already Blocked (Not Expired)",
			line: createLine(baseIP),
			setup: func(p *Processor) {
				key := TrackingKey{IPInfo: NewIPInfo(baseIP)}
				p.ActivityStore[key] = &BotActivity{
					IsBlocked:    true,
					BlockedUntil: time.Now().Add(time.Hour),
				}
			},
			assertBlockCount: 0,
			assertIsBlocked:  true,
		},
		{
			name: "Skip - Already Blocked (Out-of-Order Entry)",
			// This line has an OLDER timestamp than the one set in 'setup'.
			line: createLineWithTimestamp(baseIP, time.Now().Add(-2*time.Hour)),
			setup: func(p *Processor) {
				key := TrackingKey{IPInfo: NewIPInfo(baseIP)}
				p.ActivityStore[key] = &BotActivity{
					IsBlocked:       true,
					BlockedUntil:    time.Now().Add(time.Hour),
					LastRequestTime: time.Now().Add(-1 * time.Hour), // Last seen 1 hour ago.
				}
			},
			assertBlockCount: 0,
			assertIsBlocked:  true,
		},
		{
			name: "Process - Blocked but Expired (Should clear block state)",
			line: createLine(baseIP),
			setup: func(p *Processor) {
				key := TrackingKey{IPInfo: NewIPInfo(baseIP)}
				p.ActivityStore[key] = &BotActivity{
					IsBlocked:    true,
					BlockedUntil: time.Now().Add(-time.Hour),
				}
			},
			assertBlockCount: 0,
			assertIsBlocked:  false,
		},
		{
			name:             "Process - New IP (Should proceed to chain checks)",
			line:             createLine(baseIP),
			setup:            func(p *Processor) {},
			assertBlockCount: 0,
			assertIsBlocked:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset block count and setup store
			blockCount = 0
			p.ActivityStore = make(map[TrackingKey]*BotActivity) // Reset store for each test
			tt.setup(&p)

			p.ProcessLogLine(tt.line, 1)

			// Assertion 1: Check block call count
			blockMu.Lock()
			count := blockCount
			blockMu.Unlock()
			if count != tt.assertBlockCount {
				t.Errorf("Block count mismatch. Expected %d, got %d.", tt.assertBlockCount, count)
			}

			// Assertion 2: Check final in-memory block state for baseIP
			p.ActivityMutex.RLock()
			key := TrackingKey{IPInfo: NewIPInfo(baseIP)}
			activity, exists := p.ActivityStore[key]
			p.ActivityMutex.RUnlock()

			if tt.assertIsBlocked {
				if !exists || !activity.IsBlocked {
					t.Errorf("Expected IP %s to be blocked, but it was not.", baseIP)
				}
			} else {
				// Must ensure it's not blocked, even if it existed (e.g., in the Expired case)
				if exists && activity.IsBlocked {
					t.Errorf("Expected IP %s NOT to be blocked, but it was.", baseIP)
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
		ActivityStore:     make(map[TrackingKey]*BotActivity),
		ActivityMutex:     &sync.RWMutex{},
		Chains:            []BehavioralChain{chain},
		ChainMutex:        &sync.RWMutex{},
		Blocker:           mockBlocker,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
		Config:            &AppConfig{},
		DryRun:            true, // CRITICAL: DryRun is enabled
	}
	p.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(&p, line, lineNumber) }

	ip := "192.0.2.1"
	logLine := fmt.Sprintf(`www.example.com %s - userx [06/Nov/2025:09:00:00 +0100] "GET /1 HTTP/1.1" 200 1234 "-" "-"`, ip)
	key := TrackingKey{IPInfo: NewIPInfo(ip)}

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
