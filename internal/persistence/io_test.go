package persistence

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
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
	if len(snap.IPStates) != 1 {
		t.Errorf("Expected 1 IP state, got %d", len(snap.IPStates))
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
	if snap == nil || len(snap.IPStates) != 0 {
		t.Fatalf("Expected empty snapshot for non-existent file, got %+v", snap)
	}

	// 2. Test writing and reading a snapshot
	unblockTime := time.Now().UTC().Truncate(time.Second).Add(1 * time.Hour)
	expectedSnap := &Snapshot{
		Timestamp: time.Now().UTC().Truncate(time.Second),
		IPStates: map[string]IPState{
			"1.2.3.4": {
				State:      BlockStateBlocked,
				ExpireTime: unblockTime,
				Reason:     "test-reason",
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
	if len(loadedSnap.IPStates) != 1 {
		t.Fatalf("Expected 1 IP state, got %d", len(loadedSnap.IPStates))
	}
	state, ok := loadedSnap.IPStates["1.2.3.4"]
	if !ok {
		t.Fatalf("Expected IP 1.2.3.4 to be in snapshot")
	}
	if !state.ExpireTime.Equal(unblockTime) {
		t.Errorf("ExpireTime mismatch: got %v, want %v", state.ExpireTime, unblockTime)
	}
	if state.Reason != "test-reason" {
		t.Errorf("Reason mismatch: got %s, want test-reason", state.Reason)
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

	// Test v1 snapshot with multiple entries to verify sorting
	path := GetSnapshotPath(dir, "v1")
	now := time.Now().UTC().Truncate(time.Second)
	snap := &Snapshot{
		Version:   "v1",
		Timestamp: now,
		IPStates: map[string]IPState{
			"5.6.7.8": {
				State:      BlockStateBlocked,
				ExpireTime: now.Add(2 * time.Hour),
				Reason:     "chain2",
			},
			"1.2.3.4": {
				State:      BlockStateBlocked,
				ExpireTime: now.Add(30 * time.Minute),
				Reason:     "chain1",
			},
			"9.9.9.9": {
				State:      BlockStateBlocked,
				ExpireTime: now.Add(1 * time.Hour),
				Reason:     "chain3",
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

	// Verify the file uses v1 wrapped format with entries array
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read snapshot file: %v", err)
	}
	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	jsonData, err := io.ReadAll(gzReader)
	_ = gzReader.Close()
	if err != nil {
		t.Fatalf("Failed to decompress snapshot: %v", err)
	}
	content := string(jsonData)
	if !strings.Contains(content, `"ts"`) {
		t.Errorf("v1 snapshot missing 'ts' field: %s", content)
	}
	if !strings.Contains(content, `"snapshot"`) {
		t.Errorf("v1 snapshot missing 'snapshot' wrapper: %s", content)
	}
	if !strings.Contains(content, `"entries"`) {
		t.Errorf("v1 snapshot missing 'entries' array: %s", content)
	}
	if !strings.Contains(content, `"state"`) {
		t.Errorf("v1 snapshot missing 'state' field: %s", content)
	}
	if strings.Contains(content, `"version"`) {
		t.Errorf("v1 snapshot should not contain 'version' field: %s", content)
	}

	// Verify entries are sorted by expire_time
	var snapV1 SnapshotV1
	if err := json.Unmarshal(jsonData, &snapV1); err != nil {
		t.Fatalf("Failed to unmarshal v1 snapshot: %v", err)
	}
	if len(snapV1.Snapshot.Entries) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(snapV1.Snapshot.Entries))
	}
	// Check chronological order
	if snapV1.Snapshot.Entries[0].IP != "1.2.3.4" {
		t.Errorf("First entry should be 1.2.3.4 (earliest), got %s", snapV1.Snapshot.Entries[0].IP)
	}
	if snapV1.Snapshot.Entries[1].IP != "9.9.9.9" {
		t.Errorf("Second entry should be 9.9.9.9, got %s", snapV1.Snapshot.Entries[1].IP)
	}
	if snapV1.Snapshot.Entries[2].IP != "5.6.7.8" {
		t.Errorf("Third entry should be 5.6.7.8 (latest), got %s", snapV1.Snapshot.Entries[2].IP)
	}

	// Load and verify conversion back to map
	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if loaded.Version != "v1" {
		t.Errorf("Version mismatch: got %s, want v1", loaded.Version)
	}

	// Count blocked IPs
	blockedCount := 0
	for _, state := range loaded.IPStates {
		if state.State == BlockStateBlocked {
			blockedCount++
		}
	}
	if blockedCount != 3 {
		t.Fatalf("Expected 3 blocked IPs, got %d", blockedCount)
	}

	for ip, expectedState := range snap.IPStates {
		state, ok := loaded.IPStates[ip]
		if !ok {
			t.Errorf("Expected IP %s in loaded snapshot", ip)
			continue
		}
		if !state.ExpireTime.Equal(expectedState.ExpireTime) {
			t.Errorf("ExpireTime mismatch for %s: got %v, want %v", ip, state.ExpireTime, expectedState.ExpireTime)
		}
		if state.Reason != expectedState.Reason {
			t.Errorf("Reason mismatch for %s: got %s, want %s", ip, state.Reason, expectedState.Reason)
		}
	}

	// Verify IPStates is populated with all entries (blocked + unblocked if any)
	if len(loaded.IPStates) != 3 {
		t.Fatalf("Expected 3 IP states, got %d", len(loaded.IPStates))
	}
	for ip := range snap.IPStates {
		state, ok := loaded.IPStates[ip]
		if !ok {
			t.Errorf("Expected IP %s in IPStates", ip)
			continue
		}
		if state.State != BlockStateBlocked {
			t.Errorf("Expected state 'blocked' for %s, got %s", ip, state.State)
		}
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
	if !strings.Contains(lines, "\"type\":\"block\"") || !strings.Contains(lines, "\"ip\":\"1.1.1.1\"") {
		t.Errorf("Journal does not contain event 1: %s", lines)
	}
	if !strings.Contains(lines, "\"type\":\"unblock\"") || !strings.Contains(lines, "\"ip\":\"2.2.2.2\"") {
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

	// Test v1 journal with new naming convention and wrapped format
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

	// Verify content uses v1 wrapped format
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read journal: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "\"version\"") {
		t.Errorf("v1 journal should not contain 'version' field: %s", content)
	}
	if !strings.Contains(content, "\"ts\"") {
		t.Errorf("v1 journal missing 'ts' field: %s", content)
	}
	if !strings.Contains(content, "\"event\":{") {
		t.Errorf("v1 journal missing 'event' wrapper: %s", content)
	}
	if !strings.Contains(content, "\"type\":\"block\"") {
		t.Errorf("v1 journal should use 'type' instead of 'event': %s", content)
	}
	if !strings.Contains(content, "\"ip\":\"9.9.9.9\"") {
		t.Errorf("Journal missing IP: %s", content)
	}
	if !strings.Contains(content, "\"reason\":\"v1-chain\"") {
		t.Errorf("Journal missing reason: %s", content)
	}
}
