package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestStart_DryRun verifies that the start function correctly initiates
// the dry-run mode and processes a log file.
func TestStart_DryRun(t *testing.T) {
	resetGlobalState()

	// Create a temporary log file for the dry run.
	tmpFile, err := os.CreateTemp(t.TempDir(), "testlog-*.log")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.WriteString("dry run log line\n")
	tmpFile.Close()

	// Override the global TestLogPath.
	originalTestLogPath := TestLogPath
	TestLogPath = tmpFile.Name()
	t.Cleanup(func() { TestLogPath = originalTestLogPath })

	linesProcessed := 0
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ChainMutex:    &sync.RWMutex{},
		Chains:        []BehavioralChain{},
		Config:        &AppConfig{},
		DryRun:        true, // Enable dry-run mode.
		LogFunc:       func(level LogLevel, tag string, format string, args ...interface{}) {},
		ProcessLogLine: func(line string, lineNumber int) {
			linesProcessed++
		},
	}

	// Act: Call the start function.
	start(p)

	// Assert: Check if the log line was processed.
	if linesProcessed != 1 {
		t.Errorf("Expected 1 line to be processed in dry-run mode, but got %d", linesProcessed)
	}
}

// TestStart_LiveMode verifies that the start function correctly initiates
// the live-mode background goroutines and can be shut down gracefully.
func TestStart_LiveMode(t *testing.T) {
	resetGlobalState()

	// Create a dummy log file for the tailer to open.
	tmpFile, err := os.CreateTemp(t.TempDir(), "live-*.log")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	originalLogFilePath := LogFilePath
	LogFilePath = tmpFile.Name()
	t.Cleanup(func() { LogFilePath = originalLogFilePath })

	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ChainMutex:    &sync.RWMutex{},
		Chains:        []BehavioralChain{},
		Config: &AppConfig{
			CleanupInterval: 10 * time.Millisecond,
			PollingInterval: 10 * time.Millisecond,
		},
		DryRun:   false, // Ensure live mode.
		signalCh: make(chan os.Signal, 1),
		LogFunc:  func(level LogLevel, tag string, format string, args ...interface{}) {},
	}

	// Act: Run start in a goroutine and send a shutdown signal.
	go start(p)
	time.Sleep(20 * time.Millisecond) // Give goroutines time to start.
	p.signalCh <- syscall.SIGINT      // Send shutdown signal.
	time.Sleep(20 * time.Millisecond) // Allow for graceful shutdown.
}

// TestDryRunLogProcessor_FileOpenError verifies that DryRunLogProcessor correctly
// logs an error and exits if the test log file cannot be opened.
func TestDryRunLogProcessor_FileOpenError(t *testing.T) {
	resetGlobalState()

	// Point TestLogPath to a file that does not exist.
	nonExistentFile := "non-existent-test-log.log"
	originalTestLogPath := TestLogPath
	TestLogPath = nonExistentFile
	t.Cleanup(func() { TestLogPath = originalTestLogPath })

	// Capture log output to verify the error is logged.
	var capturedLog string
	var logMutex sync.Mutex
	logCaptureFunc := func(level LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		if tag == "FATAL" {
			capturedLog = fmt.Sprintf(format, args...)
		}
	}

	p := &Processor{
		LogFunc: logCaptureFunc,
	}
	done := make(chan struct{})

	// Act: Run the processor.
	go DryRunLogProcessor(p, done)
	<-done // Wait for it to finish.

	// Assert: Check that the correct error was logged.
	if !strings.Contains(capturedLog, "Failed to open test log file") {
		t.Fatalf("Expected a 'FATAL' log message containing 'Failed to open test log file', but got: '%s'", capturedLog)
	}
}

// TestDryRunLogProcessor_LineSkipped verifies that DryRunLogProcessor correctly
// skips lines that are too long and logs a warning.
func TestDryRunLogProcessor_LineSkipped(t *testing.T) {
	resetGlobalState()

	// Create a temporary log file with one valid line and one oversized line.
	longLine := strings.Repeat("a", MaxLogLineSize+1)
	logContent := "this is a valid line\n" + longLine + "\n"

	tmpFile, err := os.CreateTemp(t.TempDir(), "longline-*.log")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.WriteString(logContent)
	tmpFile.Close()

	originalTestLogPath := TestLogPath
	TestLogPath = tmpFile.Name()
	t.Cleanup(func() { TestLogPath = originalTestLogPath })

	// Capture log output and count processed lines.
	var capturedLog string
	var logMutex sync.Mutex
	linesProcessed := 0

	p := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {
			logMutex.Lock()
			defer logMutex.Unlock()
			if tag == "DRYRUN_SKIP" {
				capturedLog = fmt.Sprintf(format, args...)
			}
		},
		ProcessLogLine: func(line string, lineNumber int) {
			linesProcessed++
		},
	}
	done := make(chan struct{})

	go DryRunLogProcessor(p, done)
	<-done

	if linesProcessed != 1 {
		t.Errorf("Expected only 1 line to be processed, but got %d", linesProcessed)
	}
	if !strings.Contains(capturedLog, "Skipped (Length exceeded") {
		t.Errorf("Expected a 'DRYRUN_SKIP' log for oversized line, but got: '%s'", capturedLog)
	}
}
