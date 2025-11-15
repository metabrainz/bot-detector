package persistence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
