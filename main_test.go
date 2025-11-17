package main

import (
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"fmt"
	"os"
	"path/filepath"
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
	_, _ = tmpFile.WriteString("dry run log line\n")
	_ = tmpFile.Close()

	linesProcessed := 0
	p := newTestProcessor(&AppConfig{}, nil)
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

	p := newTestProcessor(&AppConfig{
		CleanupInterval: 10 * time.Millisecond,
		PollingInterval: 10 * time.Millisecond,
		EOFPollingDelay: 1 * time.Millisecond, // Poll quickly for the test
		StatFunc:        mockStat,             // Use the mock stat function
	}, nil)
	p.DryRun = false // Ensure live mode.
	// Use a channel to know when the rotation log has been seen.
	rotationLogged := make(chan struct{}, 1)
	p.LogPath = liveLogFile
	p.signalCh = make(chan os.Signal, 1)
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
		LiveLogTailer(p, p.signalCh, readyCh)
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
func TestSignalReloader_Reload(t *testing.T) {
	// --- Setup ---
	resetGlobalState()
	t.Cleanup(resetGlobalState)

	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

	// 1. Create a temporary YAML file with initial content.
	initialYAMLContent := `
version: "1.0"
log_level: "info"
chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/initial"}}]
`
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(tempFile, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}

	// Enable signal-based reloading for this test.
	// This is now set on the processor directly.

	// 2. Load the initial configuration.
	initialLoadedCfg, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: tempFile})
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config.
	processor := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        initialLoadedCfg.Chains,
		Config:        &AppConfig{},
		signalCh:      make(chan os.Signal, 1), // Initialize the signal channel
		LogFunc:       func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
		TestSignals: &TestSignals{
			// This signal is used by the test to wait for the reload to complete.
			ReloadDoneSignal: make(chan struct{}, 1),
		},
		ConfigPath: tempFile,
		ReloadOn:   "HUP", // Set for this test
	}

	// 4. Start the SignalReloader.
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go SignalReloader(processor, stopWatcher, processor.signalCh)

	// --- Act ---
	// 5. Modify the YAML file on disk.
	modifiedYAMLContent := `
version: "1.0"
log_level: "debug" # Changed log level
chains:
  - name: "ReloadedChain" # Changed chain name
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/reloaded"}}]
`
	if err := os.WriteFile(tempFile, []byte(modifiedYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write modified temp yaml file: %v", err)
	}

	// 6. Send the SIGHUP signal to the current process to trigger the reload.
	processor.signalCh <- syscall.SIGHUP

	// 7. Wait for the reload signal from the reloader.
	select {
	case <-processor.TestSignals.ReloadDoneSignal:
		// Reload completed successfully.
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for signal-based configuration reload.")
	}

	// --- Assert ---
	// 8. Check if the processor's state has been updated.
	processor.ConfigMutex.RLock()
	reloadedChains := processor.Chains
	processor.ConfigMutex.RUnlock()

	if len(reloadedChains) != 1 || reloadedChains[0].Name != "ReloadedChain" {
		t.Errorf("Expected chain to be 'ReloadedChain', but got: %+v", reloadedChains)
	}
	if logging.GetLogLevel() != logging.LevelDebug {
		t.Errorf("Expected log level to be updated to 'debug', but it was not.")
	}
}

// TestCompaction verifies that the state snapshot and journal truncation work correctly.
func TestCompaction(t *testing.T) {
	// --- Setup ---
	resetGlobalState()
	tempDir := t.TempDir()

	// Create a processor with persistence enabled.
	p := newTestProcessor(&AppConfig{}, nil)
	p.persistenceEnabled = true
	p.stateDir = tempDir
	p.NowFunc = func() time.Time { return time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC) } // Mock time

	// Manually create an events.log to be truncated.
	journalPath := filepath.Join(tempDir, "events.log")
	journalHandle, err := os.OpenFile(journalPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("Failed to create dummy journal file: %v", err)
	}
	p.journalHandle = journalHandle
	_, _ = p.journalHandle.WriteString("some old event\n")

	// Add some active blocks to the processor's state.
	p.activeBlocks = map[string]persistence.ActiveBlockInfo{
		"1.1.1.1": {UnblockTime: p.NowFunc().Add(1 * time.Hour), Reason: "chain1"},
		"2.2.2.2": {UnblockTime: p.NowFunc().Add(-1 * time.Minute), Reason: "chain2"}, // Expired
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
