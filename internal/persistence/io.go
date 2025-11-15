package persistence

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadSnapshot reads and unmarshals the snapshot file.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{ActiveBlocks: make(map[string]ActiveBlockInfo)}, nil // Return empty snapshot if not found
		}
		return nil, fmt.Errorf("failed to read snapshot file: %w", err)
	}

	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}
	if snapshot.ActiveBlocks == nil {
		snapshot.ActiveBlocks = make(map[string]ActiveBlockInfo)
	}
	return &snapshot, nil
}

// WriteSnapshot atomically writes a snapshot file.
func WriteSnapshot(path string, snap *Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temporary snapshot: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename snapshot: %w", err)
	}

	return nil
}

// OpenJournalForAppend opens the events.log file for appending.
func OpenJournalForAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

// WriteEventToJournal marshals and writes a single event to an open file handle.
func WriteEventToJournal(handle *os.File, event *AuditEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal audit event: %w", err)
	}

	if _, err := handle.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to journal: %w", err)
	}

	if err := handle.Sync(); err != nil {
		return fmt.Errorf("failed to sync journal: %w", err)
	}

	return nil
}
