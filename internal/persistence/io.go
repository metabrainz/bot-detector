package persistence

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GetJournalPath returns the journal file path (always events.log for v1).
func GetJournalPath(stateDir, version string) string {
	return filepath.Join(stateDir, "events.log")
}

// LoadSnapshot reads and unmarshals the v1 snapshot file.
// It automatically detects whether the file is gzipped or plain JSON.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{
				Version:  CurrentVersion,
				IPStates: make(map[string]IPState),
			}, nil
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

	// Parse v1 format
	var snapV1 SnapshotV1
	if err := json.Unmarshal(jsonData, &snapV1); err != nil {
		return nil, fmt.Errorf("failed to unmarshal v1 snapshot: %w", err)
	}

	// Convert entries array to IPStates map
	ipStates := make(map[string]IPState)
	// Use snapshot timestamp as ModifiedAt fallback for old snapshots without this field
	fallbackTime := snapV1.Timestamp
	if fallbackTime.IsZero() {
		fallbackTime = time.Now()
	}

	for _, entry := range snapV1.Snapshot.Entries {
		ipStates[entry.IP] = IPState{
			State:      entry.State,
			ExpireTime: entry.ExpireTime,
			Reason:     entry.Reason,
			ModifiedAt: fallbackTime, // Use snapshot timestamp, not ExpireTime
		}
	}

	return &Snapshot{
		Version:   CurrentVersion,
		Timestamp: snapV1.Timestamp,
		IPStates:  ipStates,
	}, nil
}

// WriteSnapshot atomically writes a gzipped v1 snapshot file.
func WriteSnapshot(path string, snap *Snapshot) error {
	// Use v1 wrapper format with sorted entries from IPStates
	entries := make([]BlockStateEntryV1, 0, len(snap.IPStates))
	for ip, state := range snap.IPStates {
		entries = append(entries, BlockStateEntryV1{
			IP:         ip,
			State:      state.State,
			ExpireTime: state.ExpireTime,
			Reason:     state.Reason,
		})
	}
	// Sort by ExpireTime (chronological order)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ExpireTime.Before(entries[j].ExpireTime)
	})

	snapV1 := SnapshotV1{
		Timestamp: snap.Timestamp,
		Snapshot: SnapshotDataV1{
			Entries: entries,
		},
	}
	data, err := json.Marshal(snapV1)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	// Compress the JSON data with BestSpeed for faster writes
	var buf bytes.Buffer
	gzWriter, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("failed to create gzip writer: %w", err)
	}
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
	// Use v1 wrapper format
	entry := JournalEntryV1{
		Timestamp: event.Timestamp,
		Event: AuditEventDataV1{
			Type:     event.Event,
			IP:       event.IP,
			Duration: event.Duration,
			Reason:   event.Reason,
		},
	}
	data, err := json.Marshal(entry)
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
