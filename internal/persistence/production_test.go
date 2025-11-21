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

// TestProductionV0Conversion tests loading production v0 files and converting to v1
func TestProductionV0Conversion(t *testing.T) {
	// Load production v0 snapshot
	snapshotPath := filepath.Join("..", "..", "testdata", "v0", "state.snapshot")
	snapshot, err := LoadSnapshot(snapshotPath)
	if err != nil {
		t.Fatalf("Failed to load production snapshot: %v", err)
	}

	t.Logf("Loaded snapshot with %d active blocks", len(snapshot.ActiveBlocks))
	t.Logf("Snapshot timestamp: %v", snapshot.Timestamp)
	t.Logf("Snapshot version: %s", snapshot.Version)

	// Verify it was detected as v0
	if snapshot.Version != "v0" {
		t.Errorf("Expected version v0, got %s", snapshot.Version)
	}

	// Verify IPStates was populated from ActiveBlocks
	if len(snapshot.IPStates) != len(snapshot.ActiveBlocks) {
		t.Errorf("IPStates count (%d) doesn't match ActiveBlocks count (%d)",
			len(snapshot.IPStates), len(snapshot.ActiveBlocks))
	}

	// Verify all entries are marked as blocked
	for ip, state := range snapshot.IPStates {
		if state.State != BlockStateBlocked {
			t.Errorf("IP %s should be blocked, got state %s", ip, state.State)
		}
		// Verify consistency with ActiveBlocks
		if block, ok := snapshot.ActiveBlocks[ip]; ok {
			if !state.ExpireTime.Equal(block.UnblockTime) {
				t.Errorf("IP %s: ExpireTime mismatch", ip)
			}
			if state.Reason != block.Reason {
				t.Errorf("IP %s: Reason mismatch", ip)
			}
		}
	}

	// Load and replay journal
	journalPath := filepath.Join("..", "..", "testdata", "v0", "events.log")
	journalFile, err := os.Open(journalPath)
	if err != nil {
		t.Fatalf("Failed to open journal: %v", err)
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
		var event AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Logf("Warning: Failed to parse journal event: %v", err)
			continue
		}

		if event.Timestamp.After(snapshot.Timestamp) {
			switch event.Event {
			case EventTypeBlock:
				blockCount++
				ipStates[event.IP] = IPState{
					State:      BlockStateBlocked,
					ExpireTime: event.Timestamp.Add(event.Duration),
					Reason:     event.Reason,
				}
			case EventTypeUnblock:
				unblockCount++
				ipStates[event.IP] = IPState{
					State:  BlockStateUnblocked,
					Reason: event.Reason,
				}
			}
		}
	}

	t.Logf("Replayed %d block events and %d unblock events", blockCount, unblockCount)
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

	// Verify expected counts from test data
	if len(snapshot.ActiveBlocks) != 3 {
		t.Errorf("Expected 3 blocks in snapshot, got %d", len(snapshot.ActiveBlocks))
	}
	if blockCount != 3 {
		t.Errorf("Expected 3 block events in journal, got %d", blockCount)
	}
	if unblockCount != 2 {
		t.Errorf("Expected 2 unblock events in journal, got %d", unblockCount)
	}
	if unblockedCount != 2 {
		t.Errorf("Expected 2 unblocked IPs in final state, got %d", unblockedCount)
	}

	// Write as v1 snapshot
	tempDir := t.TempDir()
	v1Path := GetSnapshotPath(tempDir, "v1")

	v1Snapshot := &Snapshot{
		Version:   "v1",
		Timestamp: time.Now().UTC(),
		IPStates:  ipStates,
	}

	// Populate ActiveBlocks for v1 write
	v1Snapshot.ActiveBlocks = make(map[string]ActiveBlockInfo)
	for ip, state := range ipStates {
		if state.State == BlockStateBlocked {
			v1Snapshot.ActiveBlocks[ip] = ActiveBlockInfo{
				UnblockTime: state.ExpireTime,
				Reason:      state.Reason,
			}
		}
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
		t.Errorf("Expected version v1, got %s", reloaded.Version)
	}

	if len(reloaded.IPStates) != len(ipStates) {
		t.Errorf("Reloaded IPStates count mismatch: got %d, want %d",
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
		t.Errorf("Unblocked count mismatch after reload: got %d, want %d",
			reloadedUnblocked, unblockedCount)
	}

	t.Logf("✓ Successfully converted v0 production data to v1")
	t.Logf("✓ v1 snapshot preserves %d unblocked IPs", reloadedUnblocked)
}
