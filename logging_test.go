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

func TestLogOutput(t *testing.T) {
	// --- Setup ---
	var logMutex sync.Mutex
	var capturedLog string
	originalLogFunc := LogOutput
	LogOutput = func(level LogLevel, tag string, format string, v ...interface{}) {
		// The mock must replicate the behavior of the real function.
		if level <= CurrentLogLevel {
			logMutex.Lock()
			capturedLog = "logged"
			logMutex.Unlock()
		}
	}
	t.Cleanup(func() {
		LogOutput = originalLogFunc
		CurrentLogLevel = LevelWarning // Reset to default
	})

	tests := []struct {
		name          string
		messageLevel  LogLevel
		currentLevel  LogLevel
		expectLogging bool
	}{
		{"Should Log - Level Warning, Current Warning", LevelWarning, LevelWarning, true},
		{"Should Log - Level Error, Current Info", LevelError, LevelInfo, true},
		{"Should NOT Log - Level Debug, Current Info", LevelDebug, LevelInfo, false},
		{"Should NOT Log - Level Info, Current Warning", LevelInfo, LevelWarning, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedLog = "" // Reset for each run
			CurrentLogLevel = tt.currentLevel
			// Call the public LogOutput function, which has been mocked, not the internal one.
			LogOutput(tt.messageLevel, "TEST", "test message")

			if (capturedLog != "") != tt.expectLogging {
				t.Errorf("Expected logging to be %v, but it was %v", tt.expectLogging, (capturedLog != ""))
			}
		})
	}
}
