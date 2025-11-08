package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestSetLogLevel(t *testing.T) {
	// --- Setup ---
	// Capture log output to verify the warning message.
	var logMutex sync.Mutex
	var capturedLog string
	originalLogFunc := LogOutput
	LogOutput = func(level LogLevel, tag string, format string, v ...interface{}) {
		logMutex.Lock()
		capturedLog = fmt.Sprintf(format, v...)
		logMutex.Unlock()
	}
	t.Cleanup(func() {
		LogOutput = originalLogFunc
		CurrentLogLevel = LevelWarning // Reset to default
	})

	// --- Test Cases ---
	tests := []struct {
		name          string
		levelStr      string
		expectedLevel LogLevel
		expectWarning bool
	}{
		{"Valid Level (debug)", "debug", LevelDebug, false},
		{"Valid Level (UPPERCASE)", "INFO", LevelInfo, false},
		{"Invalid Level", "invalid", LevelWarning, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset captured log for each run
			capturedLog = ""

			SetLogLevel(tt.levelStr)

			if CurrentLogLevel != tt.expectedLevel {
				t.Errorf("Expected CurrentLogLevel to be %v, but got %v", tt.expectedLevel, CurrentLogLevel)
			}
			if tt.expectWarning && !strings.Contains(capturedLog, "Invalid log_level") {
				t.Errorf("Expected a warning for invalid log level, but none was captured.")
			}
		})
	}
}
