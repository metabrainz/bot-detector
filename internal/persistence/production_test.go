//go:build !race

package persistence

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV1FormatRoundTrip(t *testing.T) {
	// Load v1 snapshot
	snapshotPath := filepath.Join("..", "..", "testdata", "v1", "snapshot.v1.gz")
	snapshot, err := LoadSnapshot(snapshotPath)
	if err != nil {
		t.Fatalf("Failed to load v1 snapshot: %v", err)
	}

	t.Logf("Loaded v1 snapshot with %d IP states", len(snapshot.IPStates))
	t.Logf("Snapshot timestamp: %v", snapshot.Timestamp)
	t.Logf("Snapshot version: %s", snapshot.Version)

	// Verify it was detected as v1
	if snapshot.Version != "v1" {
		t.Errorf("Expected version v1, got %s", snapshot.Version)
	}

	// Verify IPStates contains both blocked and unblocked
	if len(snapshot.IPStates) != 4 {
		t.Errorf("Expected 4 IP states in snapshot, got %d", len(snapshot.IPStates))
	}

	blockedInSnapshot := 0
	unblockedInSnapshot := 0
	for _, state := range snapshot.IPStates {
		switch state.State {
		case BlockStateBlocked:
			blockedInSnapshot++
		case BlockStateUnblocked:
			unblockedInSnapshot++
		}
	}

	if blockedInSnapshot != 2 {
		t.Errorf("Expected 2 blocked IPs in snapshot, got %d", blockedInSnapshot)
	}
	if unblockedInSnapshot != 2 {
		t.Errorf("Expected 2 unblocked IPs in snapshot, got %d", unblockedInSnapshot)
	}

	// Load and replay v1 journal
	journalPath := filepath.Join("..", "..", "testdata", "v1", "events.v1.log")
	journalFile, err := os.Open(journalPath)
	if err != nil {
		t.Fatalf("Failed to open v1 journal: %v", err)
	}
	defer func() {
		_ = journalFile.Close()
	}()

	blockCount := 0
	unblockCount := 0
	ipStates := make(map[string]IPState)

	// Copy initial state
	for ip, state := range snapshot.IPStates {
		ipStates[ip] = state
	}

	scanner := bufio.NewScanner(journalFile)
	for scanner.Scan() {
		// Parse v1 journal format
		var entry JournalEntryV1
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Logf("Warning: Failed to parse v1 journal entry: %v", err)
			continue
		}

		if entry.Timestamp.After(snapshot.Timestamp) {
			switch entry.Event.Type {
			case EventTypeBlock:
				blockCount++
				ipStates[entry.Event.IP] = IPState{
					State:      BlockStateBlocked,
					ExpireTime: entry.Timestamp.Add(entry.Event.Duration),
					Reason:     entry.Event.Reason,
				}
			case EventTypeUnblock:
				unblockCount++
				ipStates[entry.Event.IP] = IPState{
					State:  BlockStateUnblocked,
					Reason: entry.Event.Reason,
				}
			}
		}
	}

	t.Logf("Replayed %d block events and %d unblock events from v1 journal", blockCount, unblockCount)
	t.Logf("Final state: %d IPs tracked", len(ipStates))

	// Count final states
	blockedCount := 0
	unblockedCount := 0
	for _, state := range ipStates {
		if state.State == BlockStateBlocked {
			blockedCount++
		} else {
			unblockedCount++
		}
	}
	t.Logf("Final: %d blocked, %d unblocked", blockedCount, unblockedCount)

	// Verify expected counts
	if blockCount != 2 {
		t.Errorf("Expected 2 block events in v1 journal, got %d", blockCount)
	}
	if unblockCount != 1 {
		t.Errorf("Expected 1 unblock event in v1 journal, got %d", unblockCount)
	}
	if unblockedCount != 3 {
		t.Errorf("Expected 3 unblocked IPs in final state, got %d", unblockedCount)
	}

	// Write back as v1 and verify round-trip
	tempDir := t.TempDir()
	v1Path := filepath.Join(tempDir, "state.snapshot")

	v1Snapshot := &Snapshot{
		Version:   "v1",
		Timestamp: time.Now().UTC(),
		IPStates:  ipStates,
	}

	if err := WriteSnapshot(v1Path, v1Snapshot); err != nil {
		t.Fatalf("Failed to write v1 snapshot: %v", err)
	}

	// Reload and verify
	reloaded, err := LoadSnapshot(v1Path)
	if err != nil {
		t.Fatalf("Failed to reload v1 snapshot: %v", err)
	}

	if reloaded.Version != "v1" {
		t.Errorf("Expected version v1 after round-trip, got %s", reloaded.Version)
	}

	if len(reloaded.IPStates) != len(ipStates) {
		t.Errorf("Round-trip IPStates count mismatch: got %d, want %d",
			len(reloaded.IPStates), len(ipStates))
	}

	// Verify unblocked IPs are preserved
	reloadedUnblocked := 0
	for _, state := range reloaded.IPStates {
		if state.State == BlockStateUnblocked {
			reloadedUnblocked++
		}
	}

	if reloadedUnblocked != unblockedCount {
		t.Errorf("Unblocked count mismatch after round-trip: got %d, want %d",
			reloadedUnblocked, unblockedCount)
	}

	t.Logf("✓ Successfully loaded and replayed v1 format")
	t.Logf("✓ v1 round-trip preserves %d unblocked IPs", reloadedUnblocked)
}
