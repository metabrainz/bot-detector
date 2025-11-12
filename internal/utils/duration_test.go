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
		{name:="Canonical day (24h)", input: 24 * time.Hour, expected: "1d"},
		{name: "Canonical week (7d)", input: 7 * 24 * time.Hour, expected: "1w"},
		{name: "Canonical minute (60s)", input: 60 * time.Second, expected: "1m"},

		// Combinations
		{name: "Hours and minutes", input: 1*time.Hour + 30*time.Minute, expected: "1h30m"},
		{name: "Days and hours", input: 2*24*time.Hour + 6*time.Hour, expected: "2d6h"},
		{name: "Weeks and days", input: 1*7*24*time.Hour + 3*24*time.Hour, expected: "1w3d"},

		// Complex combination with all units
		{
			name: "Complex combination",
			input: (1*7*24+2*24+3)*time.Hour + 4*time.Minute + 5*time.Second,
			expected: "1w2d3h4m5s",
		},

		// Omission of zero-value units
		{
			name: "Omit zero minutes",
			input: 1*time.Hour + 5*time.Second,
			expected: "1h5s",
		},
		{
			name: "Omit zero hours",
			input: 1*24*time.Hour + 10*time.Minute,
			expected: "1d10m",
		},

		// Sub-second units
		{
			name: "Milliseconds",
			input: 123 * time.Millisecond,
			expected: "123ms",
		},
		{
			name: "Seconds and milliseconds",
			input: 1*time.Second + 50*time.Millisecond,
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