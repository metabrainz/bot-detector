package utils

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Duration
		expected string
	}{
		// Basic cases
		{name: "Zero duration", input: 0, expected: "0s"},
		{name: "Seconds", input: 10 * time.Second, expected: "10s"},
		{name: "Minutes", input: 5 * time.Minute, expected: "5m"},
		{name: "Hours", input: 2 * time.Hour, expected: "2h"},

		// Custom units (days and weeks)
		{name: "Days", input: 3 * 24 * time.Hour, expected: "3d"},
		{name: "Weeks", input: 2 * 7 * 24 * time.Hour, expected: "2w"},

		// Canonical conversions
		{name: "Canonical hour (60m)", input: 60 * time.Minute, expected: "1h"},
		{name: "Canonical day (24h)", input: 24 * time.Hour, expected: "1d"},
		{name: "Canonical week (7d)", input: 7 * 24 * time.Hour, expected: "1w"},
		{name: "Canonical week from hours (168h)", input: 168 * time.Hour, expected: "1w"},
		{name: "Canonical minute (60s)", input: 60 * time.Second, expected: "1m"},

		// Combinations
		{name: "Hours and minutes", input: 1*time.Hour + 30*time.Minute, expected: "1h30m"},
		{name: "Days and hours", input: 2*24*time.Hour + 6*time.Hour, expected: "2d6h"},
		{name: "Weeks and days", input: 1*7*24*time.Hour + 3*24*time.Hour, expected: "1w3d"},

		// Complex combination with all units
		{
			name:     "Complex combination",
			input:    (1*7*24+2*24+3)*time.Hour + 4*time.Minute + 5*time.Second,
			expected: "1w2d3h4m5s",
		},

		// Omission of zero-value units
		{
			name:     "Omit zero minutes",
			input:    1*time.Hour + 5*time.Second,
			expected: "1h5s",
		},
		{
			name:     "Omit zero hours",
			input:    1*24*time.Hour + 10*time.Minute,
			expected: "1d10m",
		},
		{
			name:     "Handle remainder from hours to days (25h)",
			input:    25 * time.Hour,
			expected: "1d1h",
		},

		// Sub-second units
		{
			name:     "Milliseconds",
			input:    123 * time.Millisecond,
			expected: "123ms",
		},
		{
			name:     "Seconds and milliseconds",
			input:    1*time.Second + 50*time.Millisecond,
			expected: "1s50ms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDuration(tt.input)
			if got != tt.expected {
				t.Errorf("FormatDuration() for input %v = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name        string
		durationStr string
		expected    time.Duration
		expectError bool
	}{
		// Standard Go durations
		{"Standard seconds", "10s", 10 * time.Second, false},
		{"Standard minutes and hours", "1h30m", 1*time.Hour + 30*time.Minute, false},
		{"Standard milliseconds", "200ms", 200 * time.Millisecond, false},

		// Day ('d') unit
		{"Single day", "1d", 24 * time.Hour, false},
		{"Multiple days", "7d", 7 * 24 * time.Hour, false},
		{"Decimal day", "1.5d", 36 * time.Hour, false},

		// Week ('w') unit
		{"Single week", "1w", 7 * 24 * time.Hour, false},
		{"Multiple weeks", "2w", 2 * 7 * 24 * time.Hour, false},
		{"Decimal week", "0.5w", 84 * time.Hour, false},

		// Combined units
		{"Week and day", "1w2d", (7*24 + 2*24) * time.Hour, false},
		{"Day and hour", "1d12h", 36 * time.Hour, false},
		{"Complex combination", "1w1d1h1m1s", (168+24+1)*time.Hour + 1*time.Minute + 1*time.Second, false},
		{"Hour and day (reversed)", "12h1d", 36 * time.Hour, false},

		// Edge cases and invalid formats
		{"No unit", "300", 0, true},
		{"Unknown unit", "1y", 0, true},
		{"Invalid combo", "1d1w", 0, true}, // Go parser doesn't like this order
		{"No number", "d", 0, true},
		{"Invalid number", "1.d", 0, true},
		{"Empty string", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDuration(tt.durationStr)

			if tt.expectError {
				if err == nil {
					t.Errorf("ParseDuration() was expected to return an error for input '%s', but it did not", tt.durationStr)
				}
			} else {
				if err != nil {
					t.Errorf("ParseDuration() returned an unexpected error for input '%s': %v", tt.durationStr, err)
				}
				if got != tt.expected {
					t.Errorf("ParseDuration() for input '%s' got %v, want %v", tt.durationStr, got, tt.expected)
				}
			}
		})
	}
}
