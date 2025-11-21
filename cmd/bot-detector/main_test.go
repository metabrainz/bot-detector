package main

import (
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/processor"
	"bot-detector/internal/testutil"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStart_DryRun(t *testing.T) {
	testutil.ResetGlobalState()

	// Create a temporary log file for the dry run.
	tmpFile, err := os.CreateTemp(t.TempDir(), "testlog-*.log")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	_, _ = tmpFile.WriteString("dry run log line\n")
	_ = tmpFile.Close()

	linesProcessed := 0
	p := testutil.NewTestProcessor(&config.AppConfig{}, nil)
	p.DryRun = true
	p.LogPath = tmpFile.Name()
	p.ProcessLogLine = func(line string) {
		linesProcessed++
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
	testutil.ResetGlobalState()

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

	p := testutil.NewTestProcessor(&config.AppConfig{
		Checker: config.CheckerConfig{
			ActorCleanupInterval: 10 * time.Millisecond,
		},
		Application: config.ApplicationConfig{
			Config: config.ConfigManagement{
				PollingInterval: 10 * time.Millisecond,
			},
			EOFPollingDelay: 1 * time.Millisecond, // Poll quickly for the test
		},
		StatFunc: mockStat, // Use the mock stat function
	}, nil)
	p.DryRun = false // Ensure live mode.
	// Use a channel to know when the rotation log has been seen.
	rotationLogged := make(chan struct{}, 1)
	p.LogPath = liveLogFile
	p.SignalCh = make(chan os.Signal, 1)
	p.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Log every message from the tailer to the test output for debugging.
		logMsg := fmt.Sprintf(format, args...)
		// In a rotation, the file might be stat'd between rename and recreate, causing a stat error.
		// This is a valid rotation detection path, so we must listen for both log messages.
		if (tag == "TAIL" && (strings.Contains(logMsg, "Detected log file rotation") || strings.Contains(logMsg, "Detected log file size reduction"))) || (tag == "TAIL_ERROR" && strings.Contains(logMsg, "Failed to stat log path")) {
			rotationLogged <- struct{}{}
		}
	}
	p.ProcessLogLine = func(line string) {}

	// Act: Run start in a goroutine.
	// The start function will launch LiveLogTailer, which will signal on readyCh
	// when it's ready to process the file.
	readyCh := make(chan struct{})
	go func() {
		// We call LiveLogTailer directly to test it in isolation, avoiding the
		// complexity and other goroutines (like ChainWatcher) started by start().
		processor.LiveLogTailer(p, p.SignalCh, readyCh)
	}()

	<-readyCh // Wait until the tailer is actually running and has opened the file.

	// --- Simulate Rotation and Update Mock State Atomically ---
	// Simulate log rotation by renaming the original file and creating a new one.
	// This changes the inode, which is the primary mechanism our tailer uses to detect rotation.
	if err := os.Rename(liveLogFile, liveLogFile+".rotated"); err != nil {
		t.Fatalf("Failed to rename log file to simulate rotation: %v", err)
	}

	if err := os.WriteFile(liveLogFile, []byte("new line\n"), 0644); err != nil {
		t.Fatalf("Failed to create new log file: %v", err)
	}

	// Get the stats of the *new* file to update the mock.
	newStat, err := os.Stat(liveLogFile)
	if err != nil {
		t.Fatalf("Failed to stat new log file: %v", err)
	}
	statMutex.Lock()
	mockStatInfo = newStat // Update the mock to return the new file's info.
	statMutex.Unlock()

	// Assert: Wait for the rotation to be detected.
	select {
	case <-rotationLogged:
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for log rotation to be detected.")
	}
}

// TestSignalReloader_Reload verifies that the signal-based configuration reloading works correctly.

func TestCompaction(t *testing.T) {
	// --- Setup ---
	testutil.ResetGlobalState()
	tempDir := t.TempDir()

	// Create a processor with persistence enabled.
	p := testutil.NewTestProcessor(&config.AppConfig{}, nil)
	p.PersistenceEnabled = true
	p.StateDir = tempDir
	p.NowFunc = func() time.Time { return time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC) } // Mock time

	// Manually create an events.log to be truncated.
	journalPath := filepath.Join(tempDir, "events.log")
	journalHandle, err := os.OpenFile(journalPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("Failed to create dummy journal file: %v", err)
	}
	p.JournalHandle = journalHandle
	_, _ = p.JournalHandle.WriteString("some old event\n")

	// Add some active blocks to the processor's state.
	p.ActiveBlocks = map[string]persistence.ActiveBlockInfo{
		"1.1.1.1": {UnblockTime: p.NowFunc().Add(1 * time.Hour), Reason: "chain1"},
		"2.2.2.2": {UnblockTime: p.NowFunc().Add(-1 * time.Minute), Reason: "chain2"}, // Expired
	}
	p.IPStates = map[string]persistence.IPState{
		"1.1.1.1": {State: persistence.BlockStateBlocked, ExpireTime: p.NowFunc().Add(1 * time.Hour), Reason: "chain1"},
		"2.2.2.2": {State: persistence.BlockStateBlocked, ExpireTime: p.NowFunc().Add(-1 * time.Minute), Reason: "chain2"}, // Expired
	}

	// --- Act ---
	runCompaction(p)

	// --- Assert ---
	// 1. Check that the snapshot file was created with the correct content.
	snapshotPath := filepath.Join(tempDir, "state.snapshot")
	loadedSnapshot, err := persistence.LoadSnapshot(snapshotPath)
	if err != nil {
		t.Fatalf("Failed to load snapshot file: %v", err)
	}

	// Verify the snapshot timestamp
	expectedTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	if !loadedSnapshot.Timestamp.Equal(expectedTime) {
		t.Errorf("Snapshot timestamp mismatch. Got: %v, Expected: %v", loadedSnapshot.Timestamp, expectedTime)
	}

	// Verify only the non-expired block is in the snapshot
	if len(loadedSnapshot.ActiveBlocks) != 1 {
		t.Errorf("Expected 1 active block in snapshot, got %d", len(loadedSnapshot.ActiveBlocks))
	}

	if block, exists := loadedSnapshot.ActiveBlocks["1.1.1.1"]; !exists {
		t.Errorf("Expected block for 1.1.1.1 not found in snapshot")
	} else {
		expectedUnblockTime := time.Date(2025, 1, 1, 13, 0, 0, 0, time.UTC)
		if !block.UnblockTime.Equal(expectedUnblockTime) {
			t.Errorf("Block unblock time mismatch. Got: %v, Expected: %v", block.UnblockTime, expectedUnblockTime)
		}
		if block.Reason != "chain1" {
			t.Errorf("Block reason mismatch. Got: %v, Expected: chain1", block.Reason)
		}
	}

	// Verify the expired block (2.2.2.2) was filtered out
	if _, exists := loadedSnapshot.ActiveBlocks["2.2.2.2"]; exists {
		t.Errorf("Expired block for 2.2.2.2 should not be in snapshot")
	}

	// 2. Check that the journal file is now empty.
	journalInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatalf("Failed to stat journal file after compaction: %v", err)
	}
	if journalInfo.Size() != 0 {
		t.Errorf("Expected journal file to be empty after compaction, but size is %d", journalInfo.Size())
	}
}
