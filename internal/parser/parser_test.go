package parser

import (
	"bot-detector/internal/utils"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// mockProvider implements the Provider interface for testing.
type mockProvider struct {
	timestampFormat string
	logRegex        *regexp.Regexp
}

func (m *mockProvider) GetTimestampFormat() string {
	return m.timestampFormat
}

func (m *mockProvider) GetLogRegex() *regexp.Regexp {
	return m.logRegex
}

func TestParseLogLine(t *testing.T) {
	testTime, _ := time.Parse("02/Jan/2006:15:04:05 -0700", "06/Nov/2025:09:00:00 +0100")

	// A standard provider for most tests.
	defaultProvider := &mockProvider{
		timestampFormat: "02/Jan/2006:15:04:05 -0700",
	}

	tests := []struct {
		name        string
		line        string
		provider    Provider
		expectError bool
		expected    *LogEntry
	}{
		{
			name:     "Valid IPv4 Line",
			line:     `www.example.com 192.168.1.1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-" "TestAgent"`,
			provider: defaultProvider,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     utils.NewIPInfo("192.168.1.1"),
				Method:     "GET",
				Path:       "/path",
				Protocol:   "HTTP/1.1",
				StatusCode: 200,
				Size:       1234,
				Referrer:   "-",
				UserAgent:  "TestAgent",
				VHost:      "www.example.com",
			},
		},
		{
			name:     "Valid IPv6 Line",
			line:     `www.example.com 2001:db8::1 - userx [06/Nov/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-" "TestAgent"`,
			provider: defaultProvider,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     utils.NewIPInfo("2001:db8::1"),
				Method:     "GET",
				Path:       "/path",
				Protocol:   "HTTP/1.1",
				StatusCode: 200,
				Size:       1234,
				Referrer:   "-",
				UserAgent:  "TestAgent",
				VHost:      "www.example.com",
			},
		},
		{
			name:     "Valid Line with Dash for Size",
			line:     `www.example.com 192.168.1.5 - userx [06/Nov/2025:09:00:00 +0100] "GET /no-content HTTP/1.1" 204 - "-" "-"`,
			provider: defaultProvider,
			expected: &LogEntry{
				Timestamp:  testTime,
				IPInfo:     utils.NewIPInfo("192.168.1.5"),
				Method:     "GET",
				Path:       "/no-content",
				Protocol:   "HTTP/1.1",
				StatusCode: 204,
				Size:       -1, // Should be parsed as -1
				Referrer:   "-",
				UserAgent:  "-",
				VHost:      "www.example.com",
			},
		},
		{
			name:        "Empty Line",
			line:        "",
			provider:    defaultProvider,
			expectError: false,
			expected:    nil,
		},
		{
			name:        "Comment Line",
			line:        "# This is a comment",
			provider:    defaultProvider,
			expectError: false,
			expected:    nil,
		},
		{
			name:        "Malformed Timestamp",
			line:        `www.example.com 192.168.1.1 - userx [06/Mal/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-" "-"`,
			provider:    defaultProvider,
			expectError: true,
		},
		{
			name:        "Invalid IP Address",
			line:        `www.example.com invalid-ip - userx [06/Nov/2025:09:00:00 +0100] "GET /path HTTP/1.1" 200 1234 "-" "-"`,
			provider:    defaultProvider,
			expectError: true,
		},
		{
			name:        "Non-matching Line",
			line:        `this line does not match the regex`,
			provider:    defaultProvider,
			expectError: true,
		},
		{
			name: "Custom Regex",
			line: `198.51.100.5 [10/Nov/2025:13:55:36 +0000] "GET /custom"`,
			provider: &mockProvider{
				timestampFormat: "02/Jan/2006:15:04:05 -0700",
				logRegex:        regexp.MustCompile(`^(?P<IP>\S+) \[(?P<Timestamp>[^\]]+)\] "(?P<Method>\S+) (?P<Path>\S+)"$`),
			},
			expected: &LogEntry{
				// Parse the expected timestamp from a string to ensure the Location matches
				// what time.Parse produces, avoiding DeepEqual issues with time.UTC.
				Timestamp: func() time.Time {
					t, _ := time.Parse("02/Jan/2006:15:04:05 -0700", "10/Nov/2025:13:55:36 +0000")
					return t
				}(),
				IPInfo: utils.NewIPInfo("198.51.100.5"),
				Method: "GET",
				Path:   "/custom",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := ParseLogLine(tt.provider, tt.line)

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
