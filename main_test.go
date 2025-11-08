package main

import (
	"os"
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
