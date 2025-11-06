package main

import (
	"strings"
	"testing"
)

func TestParseDurations(t *testing.T) {
	tests := []struct {
		name            string
		logLevel        string
		pollInterval    string
		cleanupInterval string
		idleTimeout     string
		dryRun          bool
		expectError     bool
		expectedErrMsg  string
	}{
		{
			name:            "Valid Durations and LogLevel",
			logLevel:        "info",
			pollInterval:    "10s",
			cleanupInterval: "10m",
			idleTimeout:     "1h",
			dryRun:          false,
			expectError:     false,
		},
		{
			name:           "Invalid LogLevel",
			logLevel:       "invalid",
			dryRun:         false,
			expectError:    true,
			expectedErrMsg: "invalid log-level",
		},
		{
			name:           "Invalid PollInterval",
			logLevel:       "info",
			pollInterval:   "10x",
			dryRun:         false,
			expectError:    true,
			expectedErrMsg: "invalid poll-interval",
		},
		{
			name:            "Invalid CleanupInterval",
			logLevel:        "info",
			pollInterval:    "10s",
			cleanupInterval: "10x",
			dryRun:          false,
			expectError:     true,
			expectedErrMsg:  "invalid cleanup-interval",
		},
		{
			name:            "Invalid IdleTimeout",
			logLevel:        "info",
			pollInterval:    "10s",
			cleanupInterval: "10m",
			idleTimeout:     "1x",
			dryRun:          false,
			expectError:     true,
			expectedErrMsg:  "invalid idle-timeout",
		},
		{
			name:        "DryRun Skips Duration Parsing",
			logLevel:    "info",
			dryRun:      true,
			expectError: false, // Should not error even with invalid durations
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set global flag variables for the test
			LogLevelStr = tt.logLevel
			PollingIntervalStr = tt.pollInterval
			CleanupIntervalStr = tt.cleanupInterval
			IdleTimeoutStr = tt.idleTimeout
			DryRun = tt.dryRun

			err := ParseDurations()

			if tt.expectError {
				if err == nil {
					t.Fatal("Expected an error but got nil")
				}
				if !strings.Contains(err.Error(), tt.expectedErrMsg) {
					t.Errorf("Expected error message to contain '%s', but got '%v'", tt.expectedErrMsg, err)
				}
			} else {
				if err != nil {
					t.Fatalf("Did not expect an error, but got: %v", err)
				}
			}
		})
	}
}
