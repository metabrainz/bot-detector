package persistence

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// GetSnapshotPath returns the appropriate snapshot file path for the given version.
func GetSnapshotPath(stateDir, version string) string {
	if version == "v0" {
		return filepath.Join(stateDir, "state.snapshot")
	}
	return filepath.Join(stateDir, fmt.Sprintf("snapshot.%s.gz", version))
}

// GetJournalPath returns the appropriate journal file path for the given version.
func GetJournalPath(stateDir, version string) string {
	if version == "v0" {
		return filepath.Join(stateDir, "events.log")
	}
	return filepath.Join(stateDir, fmt.Sprintf("events.%s.log", version))
}

// LoadSnapshot reads and unmarshals the snapshot file.
// It automatically detects whether the file is gzipped or plain JSON.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{Version: CurrentVersion, ActiveBlocks: make(map[string]ActiveBlockInfo)}, nil
		}
		return nil, fmt.Errorf("failed to read snapshot file: %w", err)
	}

	// Detect if the data is gzipped by checking the magic number
	var jsonData []byte
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		// File is gzipped, decompress it
		gzReader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() {
			if closeErr := gzReader.Close(); closeErr != nil {
				err = fmt.Errorf("failed to close gzip reader: %w", closeErr)
			}
		}()

		jsonData, err = io.ReadAll(gzReader)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress snapshot: %w", err)
		}
	} else {
		// File is plain JSON (backward compatibility)
		jsonData = data
	}

	var snapshot Snapshot
	if err := json.Unmarshal(jsonData, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}
	if snapshot.ActiveBlocks == nil {
		snapshot.ActiveBlocks = make(map[string]ActiveBlockInfo)
	}
	if snapshot.Version == "" {
		snapshot.Version = "v0"
	}
	return &snapshot, nil
}

// WriteSnapshot atomically writes a gzipped snapshot file.
func WriteSnapshot(path string, snap *Snapshot) error {
	if snap.Version == "" {
		snap.Version = CurrentVersion
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	// Compress the JSON data
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	if _, err := gzWriter.Write(data); err != nil {
		return fmt.Errorf("failed to gzip snapshot data: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
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
	if event.Version == "" {
		event.Version = CurrentVersion
	}
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
