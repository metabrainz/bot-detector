package persistence

import "time"

const (
	// CurrentVersion is the current persistence format version.
	CurrentVersion = "v0"
)

// PersistenceConfig holds settings for the state persistence layer.
type PersistenceConfig struct {
	Enabled            bool          `yaml:"enabled"`
	CompactionInterval time.Duration `yaml:"compaction_interval"`
}

// EventType defines the type of an audit event.
type EventType string

const (
	// EventTypeBlock represents a block action.
	EventTypeBlock EventType = "block"
	// EventTypeUnblock represents an unblock action.
	EventTypeUnblock EventType = "unblock"
)

// AuditEvent is the structure for a single entry in the journal file (v0 format).
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

// Snapshot is the structure for the state snapshot file (v0 format).
type Snapshot struct {
	Version      string                     `json:"version"`
	Timestamp    time.Time                  `json:"snapshot_time"`
	ActiveBlocks map[string]ActiveBlockInfo `json:"active_blocks"`
}

// BlockState represents the state of an IP in the blocking system.
type BlockState string

const (
	// BlockStateBlocked indicates an IP is currently blocked.
	BlockStateBlocked BlockState = "blocked"
	// BlockStateUnblocked indicates an IP is explicitly unblocked (good actor).
	BlockStateUnblocked BlockState = "unblocked"
)

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

// ActiveBlockInfo holds information about a currently active block.
type ActiveBlockInfo struct {
	UnblockTime time.Time `json:"unblock_time"`
	Reason      string    `json:"reason"`
}

// IPState represents the current state of an IP (blocked or unblocked).
type IPState struct {
	State      BlockState `json:"state"`
	ExpireTime time.Time  `json:"expire_time"`
	Reason     string     `json:"reason"`
}
