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

func TestCleanup(t *testing.T) {
	// --- Setup ---
	testutil.ResetGlobalState()

	p := testutil.NewTestProcessor(&config.AppConfig{}, nil)
	p.PersistenceEnabled = true
	p.NowFunc = func() time.Time { return time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC) }
	p.Config.Application.Persistence.RetentionPeriod = 24 * time.Hour

	db, err := persistence.OpenDB("", true)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer func() { _ = persistence.CloseDB(db) }()
	p.DB = db

	now := p.NowFunc()
	// Active block
	_ = persistence.UpsertIPState(db, "1.1.1.1", persistence.BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, now)
	// Expired block
	_ = persistence.UpsertIPState(db, "2.2.2.2", persistence.BlockStateBlocked, now.Add(-1*time.Minute), "chain2", now, now)
	// Old unblocked (past retention)
	_ = persistence.UpsertIPState(db, "3.3.3.3", persistence.BlockStateUnblocked, now.Add(-48*time.Hour), "good-actor", now, time.Time{})

	// --- Act ---
	runCleanup(p)

	// --- Assert ---
	states, err := persistence.GetAllIPStates(db)
	if err != nil {
		t.Fatalf("Failed to get states: %v", err)
	}

	if _, exists := states["1.1.1.1"]; !exists {
		t.Error("Expected active block 1.1.1.1 to be kept")
	}
	if _, exists := states["2.2.2.2"]; exists {
		t.Error("Expected expired block 2.2.2.2 to be cleaned up")
	}
	if _, exists := states["3.3.3.3"]; exists {
		t.Error("Expected old unblocked 3.3.3.3 to be cleaned up")
	}
}

func TestCorruptedJournalMigration(t *testing.T) {
	// Test that migration handles a corrupted journal gracefully,
	// processing valid entries and skipping corrupted ones.
	tempDir := t.TempDir()

	// Create a snapshot via the legacy writer
	snapshotPath := filepath.Join(tempDir, "state.snapshot")
	snapshot := &persistence.Snapshot{
		Version:   "v1",
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

	// Create a journal with valid v1 entries and a corrupted entry
	journalPath := filepath.Join(tempDir, "events.log")
	journalContent := `{"ts":"2025-01-01T12:00:01Z","event":{"type":"block","ip":"2.2.2.2","duration":3600000000000,"reason":"valid-block"}}
{"ts":"2025-01-01T12:00:02Z","event":{"type":"unblock","ip":"1.1.1.1","reason":"good-actor"}}
{"ts":"2025-01-01T12:00:03Z","event":{"type":"block","ip":"4.4.4.4","duration":1800000000000,"reason":"truncated
`
	if err := os.WriteFile(journalPath, []byte(journalContent), 0644); err != nil {
		t.Fatalf("Failed to write journal: %v", err)
	}

	// Migrate to SQLite
	db, err := persistence.OpenDB("", true)
	if err != nil {
		t.Fatalf("Failed to open DB: %v", err)
	}
	defer func() { _ = persistence.CloseDB(db) }()

	err = persistence.MigrateFromLegacy(db, tempDir)
	if err != nil {
		t.Fatalf("Migration should succeed despite corrupted journal entry: %v", err)
	}

	// Verify valid entries were migrated
	states, err := persistence.GetAllIPStates(db)
	if err != nil {
		t.Fatalf("Failed to get states: %v", err)
	}

	// 1.1.1.1 should be unblocked (journal overrides snapshot)
	if state, exists := states["1.1.1.1"]; !exists {
		t.Error("Expected 1.1.1.1 in state")
	} else if state.State != persistence.BlockStateUnblocked {
		t.Errorf("Expected 1.1.1.1 to be unblocked, got %s", state.State)
	}

	// 2.2.2.2 should be blocked
	if _, exists := states["2.2.2.2"]; !exists {
		t.Error("Expected 2.2.2.2 in state (valid block)")
	}

	// 4.4.4.4 should NOT be present (corrupted entry)
	if _, exists := states["4.4.4.4"]; exists {
		t.Error("Corrupted entry for 4.4.4.4 should not have been processed")
	}
}
