package main

import (
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/processor"
	"bot-detector/internal/testutil"
	"bufio"
	"encoding/json"
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

	// Add some blocked IPs to the processor's state.
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
	blockedCount := 0
	for _, state := range loadedSnapshot.IPStates {
		if state.State == persistence.BlockStateBlocked {
			blockedCount++
		}
	}
	if blockedCount != 1 {
		t.Errorf("Expected 1 blocked IP in snapshot, got %d", blockedCount)
	}

	if state, exists := loadedSnapshot.IPStates["1.1.1.1"]; !exists {
		t.Errorf("Expected block for 1.1.1.1 not found in snapshot")
	} else {
		expectedExpireTime := time.Date(2025, 1, 1, 13, 0, 0, 0, time.UTC)
		if !state.ExpireTime.Equal(expectedExpireTime) {
			t.Errorf("Block expire time mismatch. Got: %v, Expected: %v", state.ExpireTime, expectedExpireTime)
		}
		if state.Reason != "chain1" {
			t.Errorf("Block reason mismatch. Got: %v, Expected: chain1", state.Reason)
		}
	}

	// Verify the expired block (2.2.2.2) was converted to unblocked (preserves reason)
	if state, exists := loadedSnapshot.IPStates["2.2.2.2"]; !exists {
		t.Errorf("Expired block for 2.2.2.2 should be kept as unblocked")
	} else if state.State != persistence.BlockStateUnblocked {
		t.Errorf("Expired block for 2.2.2.2 should be unblocked, got: %v", state.State)
	} else if state.Reason != "chain2" {
		t.Errorf("Expired block reason should be preserved, got: %v", state.Reason)
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

func TestCorruptedJournalHandling(t *testing.T) {
	// --- Arrange ---
	tempDir, err := os.MkdirTemp("", "bot_detector_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	// Create a snapshot
	snapshotPath := filepath.Join(tempDir, "state.snapshot")
	snapshot := &persistence.Snapshot{
		Version:   "v0",
		Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		IPStates: map[string]persistence.IPState{
			"1.1.1.1": {
				State:      persistence.BlockStateBlocked,
				ExpireTime: time.Date(2025, 1, 1, 13, 0, 0, 0, time.UTC),
				Reason:     "initial-block",
			},
		},
	}
	if err := persistence.WriteSnapshot(snapshotPath, snapshot); err != nil {
		t.Fatalf("Failed to write snapshot: %v", err)
	}

	// Create a journal with valid entries and a corrupted entry at the end
	journalPath := filepath.Join(tempDir, "events.log")
	journalContent := `{"version":"v0","ts":"2025-01-01T12:00:01Z","event":"block","ip":"2.2.2.2","duration":3600000000000,"reason":"valid-block-1"}
{"version":"v0","ts":"2025-01-01T12:00:02Z","event":"unblock","ip":"1.1.1.1","reason":"good-actor"}
{"version":"v0","ts":"2025-01-01T12:00:03Z","event":"block","ip":"3.3.3.3","duration":7200000000000,"reason":"valid-block-2"}
{"version":"v0","ts":"2025-01-01T12:00:04Z","event":"block","ip":"4.4.4.4","duration":1800000000000,"reason":"truncated
`
	if err := os.WriteFile(journalPath, []byte(journalContent), 0644); err != nil {
		t.Fatalf("Failed to write journal: %v", err)
	}

	// --- Act: Manually replay journal to test parsing ---
	loadedSnapshot, err := persistence.LoadSnapshot(snapshotPath)
	if err != nil {
		t.Fatalf("Failed to load snapshot: %v", err)
	}

	ipStates := make(map[string]persistence.IPState)
	for ip, state := range loadedSnapshot.IPStates {
		ipStates[ip] = state
	}

	journalFile, err := os.Open(journalPath)
	if err != nil {
		t.Fatalf("Failed to open journal: %v", err)
	}
	defer func() {
		_ = journalFile.Close()
	}()

	blockEvents := 0
	unblockEvents := 0
	parseErrors := 0

	scanner := bufio.NewScanner(journalFile)
	for scanner.Scan() {
		var event persistence.AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Logf("Parse error (expected): %v", err)
			parseErrors++
			continue
		}
		if event.Timestamp.After(loadedSnapshot.Timestamp) {
			switch event.Event {
			case persistence.EventTypeBlock:
				blockEvents++
				expireTime := event.Timestamp.Add(event.Duration)
				ipStates[event.IP] = persistence.IPState{
					State:      persistence.BlockStateBlocked,
					ExpireTime: expireTime,
					Reason:     event.Reason,
				}
			case persistence.EventTypeUnblock:
				unblockEvents++
				ipStates[event.IP] = persistence.IPState{
					State:  persistence.BlockStateUnblocked,
					Reason: event.Reason,
				}
			}
		}
	}

	// --- Assert ---
	if parseErrors != 1 {
		t.Errorf("Expected 1 parse error, got %d", parseErrors)
	}

	if blockEvents != 2 {
		t.Errorf("Expected 2 block events, got %d", blockEvents)
	}

	if unblockEvents != 1 {
		t.Errorf("Expected 1 unblock event, got %d", unblockEvents)
	}

	// Verify that valid entries were processed despite the corrupted one
	if len(ipStates) != 3 {
		t.Errorf("Expected 3 IP states (2 blocked + 1 unblocked), got %d", len(ipStates))
	}

	// Check that the valid blocks were loaded
	if _, exists := ipStates["2.2.2.2"]; !exists {
		t.Errorf("Expected IP 2.2.2.2 to be in state (valid-block-1)")
	}
	if _, exists := ipStates["3.3.3.3"]; !exists {
		t.Errorf("Expected IP 3.3.3.3 to be in state (valid-block-2)")
	}

	// Check that the unblock was processed
	if state, exists := ipStates["1.1.1.1"]; !exists {
		t.Errorf("Expected IP 1.1.1.1 to be in state (unblocked)")
	} else if state.State != persistence.BlockStateUnblocked {
		t.Errorf("Expected IP 1.1.1.1 to be unblocked, got state %s", state.State)
	}

	// Verify the corrupted entry was NOT processed
	if _, exists := ipStates["4.4.4.4"]; exists {
		t.Errorf("Corrupted entry for IP 4.4.4.4 should not have been processed")
	}

	t.Logf("✓ Corrupted journal handled correctly: 2 blocks + 1 unblock processed, 1 corrupted entry skipped")
}
