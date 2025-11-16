package logging

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestSetLogLevel(t *testing.T) {
	// --- Setup ---
	// Capture log output to verify the warning message.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		currentLogLevel = LevelWarning // Reset to default
		log.SetOutput(os.Stderr)       // Restore original log output
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
			buf.Reset()

			SetLogLevel(tt.levelStr)

			logMutex.RLock()
			level := currentLogLevel
			logMutex.RUnlock()

			if level != tt.expectedLevel {
				t.Errorf("Expected currentLogLevel to be %v, but got %v", tt.expectedLevel, level)
			}
			if tt.expectWarning && !strings.Contains(buf.String(), "Invalid log_level") {
				t.Errorf("Expected a warning for invalid log level, but none was captured.")
			}
		})
	}
}

func TestLogOutput(t *testing.T) {
	// --- Setup ---
	originalLogFunc := LogOutput
	LogOutput = func(level LogLevel, tag string, format string, v ...interface{}) {
		// The mock must replicate the behavior of the real function.
		// We call the internal function directly to test it.
		logOutputInternal(level, tag, format, v...)
	}
	// The actual output is controlled by the standard logger, which we will capture.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		LogOutput = originalLogFunc
		currentLogLevel = LevelWarning // Reset to default
		log.SetOutput(os.Stderr)       // Restore original log output
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
			buf.Reset() // Reset buffer for each run
			currentLogLevel = tt.currentLevel
			// Call the public LogOutput function, which has been mocked, not the internal one.
			LogOutput(tt.messageLevel, "TEST", "test message")

			if (buf.Len() > 0) != tt.expectLogging {
				t.Errorf("Expected logging to be %v, but it was %v", tt.expectLogging, (buf.Len() > 0))
			}
		})
	}
}
