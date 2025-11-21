package persistence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionedFilePaths(t *testing.T) {
	tests := []struct {
		version      string
		wantSnapshot string
		wantJournal  string
	}{
		{"v0", "state.snapshot", "events.log"},
		{"v1", "snapshot.v1.gz", "events.v1.log"},
		{"v2", "snapshot.v2.gz", "events.v2.log"},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			gotSnapshot := GetSnapshotPath("/tmp", tt.version)
			gotJournal := GetJournalPath("/tmp", tt.version)

			if filepath.Base(gotSnapshot) != tt.wantSnapshot {
				t.Errorf("GetSnapshotPath(%s) = %s, want %s", tt.version, gotSnapshot, tt.wantSnapshot)
			}
			if filepath.Base(gotJournal) != tt.wantJournal {
				t.Errorf("GetJournalPath(%s) = %s, want %s", tt.version, gotJournal, tt.wantJournal)
			}
		})
	}
}

func TestV0BackwardCompatibility(t *testing.T) {
	dir, err := os.MkdirTemp("", "persistence_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
	}()

	// Write a v0 snapshot without version field (simulating old format)
	path := filepath.Join(dir, "state.snapshot")
	oldFormatJSON := `{
  "snapshot_time": "2025-11-21T12:00:00Z",
  "active_blocks": {
    "1.2.3.4": {
      "unblock_time": "2025-11-21T13:00:00Z",
      "reason": "test"
    }
  }
}`
	if err := os.WriteFile(path, []byte(oldFormatJSON), 0644); err != nil {
		t.Fatalf("Failed to write old format snapshot: %v", err)
	}

	// Load it and verify it defaults to v0
	snap, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if snap.Version != "v0" {
		t.Errorf("Expected version v0 for old format, got %s", snap.Version)
	}
	if len(snap.ActiveBlocks) != 1 {
		t.Errorf("Expected 1 active block, got %d", len(snap.ActiveBlocks))
	}
}

func TestSnapshotting(t *testing.T) {
	dir, err := os.MkdirTemp("", "persistence_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
	}()

	path := filepath.Join(dir, "state.snapshot")

	// 1. Test loading a non-existent snapshot
	snap, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed for non-existent file: %v", err)
	}
	if snap == nil || len(snap.ActiveBlocks) != 0 {
		t.Fatalf("Expected empty snapshot for non-existent file, got %+v", snap)
	}

	// 2. Test writing and reading a snapshot
	unblockTime := time.Now().UTC().Truncate(time.Second).Add(1 * time.Hour)
	expectedSnap := &Snapshot{
		Timestamp: time.Now().UTC().Truncate(time.Second),
		ActiveBlocks: map[string]ActiveBlockInfo{
			"1.2.3.4": {
				UnblockTime: unblockTime,
				Reason:      "test-reason",
			},
		},
	}

	if err := WriteSnapshot(path, expectedSnap); err != nil {
		t.Fatalf("WriteSnapshot failed: %v", err)
	}

	// Verify .tmp file is gone
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("Expected .tmp file to be removed, but it exists")
	}

	loadedSnap, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed after writing: %v", err)
	}

	if !loadedSnap.Timestamp.Equal(expectedSnap.Timestamp) {
		t.Errorf("Timestamp mismatch: got %v, want %v", loadedSnap.Timestamp, expectedSnap.Timestamp)
	}
	if len(loadedSnap.ActiveBlocks) != 1 {
		t.Fatalf("Expected 1 active block, got %d", len(loadedSnap.ActiveBlocks))
	}
	info, ok := loadedSnap.ActiveBlocks["1.2.3.4"]
	if !ok {
		t.Fatalf("Expected IP 1.2.3.4 to be in snapshot")
	}
	if !info.UnblockTime.Equal(unblockTime) {
		t.Errorf("UnblockTime mismatch: got %v, want %v", info.UnblockTime, unblockTime)
	}
	if info.Reason != "test-reason" {
		t.Errorf("Reason mismatch: got %s, want test-reason", info.Reason)
	}
}

func TestV1SnapshotFormat(t *testing.T) {
	dir, err := os.MkdirTemp("", "persistence_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
	}()

	// Test v1 snapshot with new naming convention
	path := GetSnapshotPath(dir, "v1")
	unblockTime := time.Now().UTC().Truncate(time.Second).Add(1 * time.Hour)
	snap := &Snapshot{
		Version:   "v1",
		Timestamp: time.Now().UTC().Truncate(time.Second),
		ActiveBlocks: map[string]ActiveBlockInfo{
			"5.6.7.8": {
				UnblockTime: unblockTime,
				Reason:      "v1-test",
			},
		},
	}

	if err := WriteSnapshot(path, snap); err != nil {
		t.Fatalf("WriteSnapshot failed: %v", err)
	}

	// Verify file name is correct
	if filepath.Base(path) != "snapshot.v1.gz" {
		t.Errorf("Expected filename snapshot.v1.gz, got %s", filepath.Base(path))
	}

	// Load and verify
	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if loaded.Version != "v1" {
		t.Errorf("Version mismatch: got %s, want v1", loaded.Version)
	}
	if len(loaded.ActiveBlocks) != 1 {
		t.Fatalf("Expected 1 active block, got %d", len(loaded.ActiveBlocks))
	}
	info, ok := loaded.ActiveBlocks["5.6.7.8"]
	if !ok {
		t.Fatalf("Expected IP 5.6.7.8 in snapshot")
	}
	if info.Reason != "v1-test" {
		t.Errorf("Reason mismatch: got %s, want v1-test", info.Reason)
	}
}

func TestJournaling(t *testing.T) {
	dir, err := os.MkdirTemp("", "persistence_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
	}()

	path := filepath.Join(dir, "events.log")

	handle, err := OpenJournalForAppend(path)
	if err != nil {
		t.Fatalf("OpenJournalForAppend failed: %v", err)
	}

	event1 := &AuditEvent{
		Timestamp: time.Now().UTC().Truncate(time.Second),
		Event:     EventTypeBlock,
		IP:        "1.1.1.1",
		Duration:  5 * time.Minute,
		Reason:    "chain1",
	}
	event2 := &AuditEvent{
		Timestamp: time.Now().UTC().Truncate(time.Second).Add(1 * time.Second),
		Event:     EventTypeUnblock,
		IP:        "2.2.2.2",
		Reason:    "good-actor",
	}

	if err := WriteEventToJournal(handle, event1); err != nil {
		t.Fatalf("WriteEventToJournal failed for event 1: %v", err)
	}
	if err := WriteEventToJournal(handle, event2); err != nil {
		t.Fatalf("WriteEventToJournal failed for event 2: %v", err)
	}
	_ = handle.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read journal file: %v", err)
	}

	lines := string(data)
	if !strings.Contains(lines, "\"event\":\"block\"") || !strings.Contains(lines, "\"ip\":\"1.1.1.1\"") {
		t.Errorf("Journal does not contain event 1: %s", lines)
	}
	if !strings.Contains(lines, "\"event\":\"unblock\"") || !strings.Contains(lines, "\"ip\":\"2.2.2.2\"") {
		t.Errorf("Journal does not contain event 2: %s", lines)
	}
	if !strings.HasSuffix(lines, "\n") {
		t.Errorf("Journal does not end with a newline")
	}
}

func TestV1JournalFormat(t *testing.T) {
	dir, err := os.MkdirTemp("", "persistence_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
	}()

	// Test v1 journal with new naming convention
	path := GetJournalPath(dir, "v1")
	handle, err := OpenJournalForAppend(path)
	if err != nil {
		t.Fatalf("OpenJournalForAppend failed: %v", err)
	}

	event := &AuditEvent{
		Version:   "v1",
		Timestamp: time.Now().UTC().Truncate(time.Second),
		Event:     EventTypeBlock,
		IP:        "9.9.9.9",
		Duration:  10 * time.Minute,
		Reason:    "v1-chain",
	}

	if err := WriteEventToJournal(handle, event); err != nil {
		t.Fatalf("WriteEventToJournal failed: %v", err)
	}
	_ = handle.Close()

	// Verify file name is correct
	if filepath.Base(path) != "events.v1.log" {
		t.Errorf("Expected filename events.v1.log, got %s", filepath.Base(path))
	}

	// Verify content
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read journal: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "\"version\":\"v1\"") {
		t.Errorf("Journal missing version field: %s", content)
	}
	if !strings.Contains(content, "\"ip\":\"9.9.9.9\"") {
		t.Errorf("Journal missing IP: %s", content)
	}
	if !strings.Contains(content, "\"reason\":\"v1-chain\"") {
		t.Errorf("Journal missing reason: %s", content)
	}
}
