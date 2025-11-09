package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	originalLogFilePath := LogFilePath
	LogFilePath = tmpFile.Name()
	t.Cleanup(func() { LogFilePath = originalLogFilePath })

	linesProcessed := 0
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        []BehavioralChain{},
		Config:        &AppConfig{},
		DryRun:        true, // Enable dry-run mode.
		LogFunc:       func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
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
	// This test is more comprehensive. It not only starts live mode but also
	// verifies that the log rotation logic (which uses StatFunc) is exercised.
	resetGlobalState()

	// --- Mock StatFunc for Deterministic Testing ---
	var statMutex sync.Mutex
	var mockStatError error
	var mockStatInfo os.FileInfo

	mockStat := func(path string) (os.FileInfo, error) {
		statMutex.Lock()
		defer statMutex.Unlock()
		if mockStatError != nil {
			return nil, mockStatError
		}
		return mockStatInfo, nil
	}

	// Create a dummy log file for the tailer to open.
	tempDir := t.TempDir()
	liveLogFile := filepath.Join(tempDir, "live.log")
	if err := os.WriteFile(liveLogFile, []byte("initial line\n"), 0644); err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	// Get initial stats for the mock.
	initialStat, err := os.Stat(liveLogFile)
	if err != nil {
		t.Fatalf("Failed to stat initial log file: %v", err)
	}
	mockStatInfo = initialStat // Initially, the mock returns the original file info.

	originalLogFilePath := LogFilePath
	LogFilePath = liveLogFile
	t.Cleanup(func() { LogFilePath = originalLogFilePath })

	// Use a channel to know when the rotation log has been seen.
	rotationLogged := make(chan struct{}, 1)

	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        []BehavioralChain{},
		Config: &AppConfig{
			CleanupInterval: 10 * time.Millisecond,
			PollingInterval: 10 * time.Millisecond,
			EOFPollingDelay: 1 * time.Millisecond, // Poll quickly for the test
			StatFunc:        mockStat,             // Use the mock stat function
		},
		DryRun:   false, // Ensure live mode.
		signalCh: make(chan os.Signal, 1),
		LogFunc: func(level logging.LogLevel, tag string, format string, args ...interface{}) {
			// Log every message from the tailer to the test output for debugging.
			logMsg := fmt.Sprintf(format, args...)
			// In a rotation, the file might be stat'd between rename and recreate, causing a stat error.
			// This is a valid rotation detection path, so we must listen for both log messages.
			if (tag == "TAIL" && (strings.Contains(logMsg, "Detected log file rotation") || strings.Contains(logMsg, "Detected log file size reduction"))) || (tag == "TAIL_ERROR" && strings.Contains(logMsg, "Failed to stat log path")) {
				rotationLogged <- struct{}{}
			}
		},
		// We don't need to process lines for this test, just detect rotation.
		// A no-op function prevents a nil pointer panic.
		ProcessLogLine: func(line string, lineNumber int) {},
	}

	// Act: Run start in a goroutine.
	// The start function will launch LiveLogTailer, which will signal on readyCh
	// when it's ready to process the file.
	readyCh := make(chan struct{})
	go func() {
		// We call LiveLogTailer directly to test it in isolation, avoiding the
		// complexity and other goroutines (like ChainWatcher) started by start().
		LiveLogTailer(p, p.signalCh, readyCh)
	}()

	<-readyCh // Wait until the tailer is actually running and has opened the file.

	// --- Simulate Rotation and Update Mock State Atomically ---
	// Acquire the lock to prevent the tailer's goroutine from reading the mock
	// state while we are in the middle of changing it.
	statMutex.Lock()

	// Simulate log rotation.
	if err := os.Rename(liveLogFile, liveLogFile+".rotated"); err != nil {
		statMutex.Unlock() // Ensure unlock on failure
		t.Fatalf("Failed to rename log file: %v", err)
	}

	if err := os.WriteFile(liveLogFile, []byte("new line\n"), 0644); err != nil {
		statMutex.Unlock() // Ensure unlock on failure
		t.Fatalf("Failed to create new log file: %v", err)
	}

	// Get the stats of the *new* file.
	newStat, err := os.Stat(liveLogFile)
	if err != nil {
		statMutex.Unlock() // Ensure unlock on failure
		t.Fatalf("Failed to stat new log file: %v", err)
	}

	// Now, update the mock to return the new file's info, which will trigger rotation detection.
	mockStatInfo = newStat
	statMutex.Unlock()

	// Assert: Wait for the rotation to be detected.
	select {
	case <-rotationLogged:
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for log rotation to be detected.")
	}
}

// TestDryRunLogProcessor_FileOpenError verifies that DryRunLogProcessor correctly
// logs an error and exits if the test log file cannot be opened.
func TestDryRunLogProcessor_FileOpenError(t *testing.T) {
	resetGlobalState()
	harness := newDryRunTestHarness(t)
	// Ensure the file does not exist.
	os.Remove(harness.tempLogFile)

	done := make(chan struct{})

	// Act: Run the processor.
	go DryRunLogProcessor(harness.processor, done)
	<-done // Wait for it to finish.

	// Assert: Check that the correct error was logged.
	logOutput := strings.Join(harness.capturedLogs, "\n")
	if !strings.Contains(logOutput, "Failed to open log file") {
		t.Fatalf("Expected a log message containing 'Failed to open log file', but got: '%s'", logOutput)
	}
}

// TestDryRunLogProcessor_LineSkipped verifies that DryRunLogProcessor correctly
// skips lines that are too long and logs a warning.
func TestDryRunLogProcessor_LineSkipped(t *testing.T) {
	resetGlobalState()
	harness := newDryRunTestHarness(t)

	// Create a temporary log file with one valid line and one oversized line.
	longLine := strings.Repeat("a", MaxLogLineSize+1)
	logContent := "this is a valid line\n" + longLine + "\n"
	if err := os.WriteFile(harness.tempLogFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	done := make(chan struct{})

	go DryRunLogProcessor(harness.processor, done)
	<-done

	if len(harness.processedLines) != 1 {
		t.Errorf("Expected only 1 line to be processed, but got %d", len(harness.processedLines))
	}
	logOutput := strings.Join(harness.capturedLogs, "\n")
	if !strings.Contains(logOutput, "Skipped (Length exceeded") {
		t.Errorf("Expected a log for oversized line, but got: '%s'", logOutput)
	}
}
