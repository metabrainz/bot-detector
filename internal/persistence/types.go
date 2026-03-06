package persistence

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	// CurrentVersion is the current persistence format version.
	CurrentVersion = "v1"
)

// PersistenceConfig holds settings for the state persistence layer.
type PersistenceConfig struct {
	Enabled            *bool         `yaml:"enabled"`
	CompactionInterval time.Duration `yaml:"compaction_interval"`
	RetentionPeriod    time.Duration `yaml:"retention_period"` // How long to keep unblocked entries
}

// EventType defines the type of an audit event.
type EventType string

const (
	// EventTypeBlock represents a block action.
	EventTypeBlock EventType = "block"
	// EventTypeUnblock represents an unblock action.
	EventTypeUnblock EventType = "unblock"
)

// AuditEvent is used internally for writing journal entries.
type AuditEvent struct {
	Version   string        `json:"version"`
	Timestamp time.Time     `json:"ts"`
	Event     EventType     `json:"event"`
	IP        string        `json:"ip"`
	Duration  time.Duration `json:"duration,omitempty"`
	Reason    string        `json:"reason,omitempty"`
}

// AuditEventDataV1 is the event data structure for v1 format (without timestamp).
type AuditEventDataV1 struct {
	Type     EventType     `json:"type"`
	IP       string        `json:"ip"`
	Duration time.Duration `json:"duration,omitempty"`
	Reason   string        `json:"reason,omitempty"`
}

// JournalEntryV1 is the wrapper structure for v1 journal entries.
type JournalEntryV1 struct {
	Timestamp time.Time        `json:"ts"`
	Event     AuditEventDataV1 `json:"event"`
}

// Snapshot is the internal structure for the state snapshot.
type Snapshot struct {
	Version   string             `json:"version,omitempty"`
	Timestamp time.Time          `json:"snapshot_time"`
	IPStates  map[string]IPState `json:"-"` // Not serialized, populated from v1 entries array
}

// BlockState represents the state of an IP in the blocking system.
type BlockState uint8

const (
	// BlockStateUnblocked indicates an IP is explicitly unblocked (good actor).
	BlockStateUnblocked BlockState = 0
	// BlockStateBlocked indicates an IP is currently blocked.
	BlockStateBlocked BlockState = 1
)

// String returns the string representation of the block state.
func (bs BlockState) String() string {
	switch bs {
	case BlockStateBlocked:
		return "blocked"
	case BlockStateUnblocked:
		return "unblocked"
	default:
		return "unknown"
	}
}

// MarshalJSON implements json.Marshaler for BlockState.
func (bs BlockState) MarshalJSON() ([]byte, error) {
	return json.Marshal(bs.String())
}

// UnmarshalJSON implements json.Unmarshaler for BlockState.
func (bs *BlockState) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "blocked":
		*bs = BlockStateBlocked
	case "unblocked":
		*bs = BlockStateUnblocked
	default:
		return fmt.Errorf("invalid block state: %s", s)
	}
	return nil
}

// BlockStateEntryV1 represents a single IP state entry in v1 snapshots.
type BlockStateEntryV1 struct {
	IP         string     `json:"ip"`
	State      BlockState `json:"state"`
	ExpireTime time.Time  `json:"expire_time"`
	Reason     string     `json:"reason"`
}

// SnapshotDataV1 is the snapshot data structure for v1 format (without timestamp).
type SnapshotDataV1 struct {
	Entries []BlockStateEntryV1 `json:"entries"`
}

// SnapshotV1 is the wrapper structure for v1 snapshots.
type SnapshotV1 struct {
	Timestamp time.Time      `json:"ts"`
	Snapshot  SnapshotDataV1 `json:"snapshot"`
}

// IPState represents the current state of an IP (blocked or unblocked).
type IPState struct {
	State      BlockState `json:"state"`
	ExpireTime time.Time  `json:"expire_time"`
	Reason     string     `json:"reason"`
}
