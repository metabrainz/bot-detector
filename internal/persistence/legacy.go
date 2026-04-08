package persistence

// Legacy snapshot reader/writer — kept for migration from old format.
// These functions are used by migrate.go and migration tests only.
// Do not use for new code.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// LoadSnapshot reads and unmarshals a legacy v1 snapshot file.
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

	var jsonData []byte
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
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
		jsonData = data
	}

	var snapV1 SnapshotV1
	if err := json.Unmarshal(jsonData, &snapV1); err != nil {
		return nil, fmt.Errorf("failed to unmarshal v1 snapshot: %w", err)
	}

	ipStates := make(map[string]IPState)
	fallbackTime := snapV1.Timestamp
	if fallbackTime.IsZero() {
		fallbackTime = time.Now()
	}

	for _, entry := range snapV1.Snapshot.Entries {
		ipStates[entry.IP] = IPState{
			State:      entry.State,
			ExpireTime: entry.ExpireTime,
			Reason:     entry.Reason,
			ModifiedAt: fallbackTime,
		}
	}

	return &Snapshot{
		Version:   CurrentVersion,
		Timestamp: snapV1.Timestamp,
		IPStates:  ipStates,
	}, nil
}

// WriteSnapshot atomically writes a gzipped v1 snapshot file.
// Kept for migration tests that need to create legacy fixture files.
func WriteSnapshot(path string, snap *Snapshot) error {
	entries := make([]BlockStateEntryV1, 0, len(snap.IPStates))
	for ip, state := range snap.IPStates {
		entries = append(entries, BlockStateEntryV1{
			IP:         ip,
			State:      state.State,
			ExpireTime: state.ExpireTime,
			Reason:     state.Reason,
		})
	}
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

	return os.Rename(tmpPath, path)
}
