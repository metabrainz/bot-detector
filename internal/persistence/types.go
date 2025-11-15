package persistence

import "time"

// PersistenceConfig holds settings for the state persistence layer.
type PersistenceConfig struct {
	Enabled            bool          `yaml:"enabled"`
	StateDir           string        `yaml:"state_dir"`
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

// AuditEvent is the structure for a single entry in the journal file.
type AuditEvent struct {
	Timestamp time.Time     `json:"ts"`
	Event     EventType     `json:"event"`
	IP        string        `json:"ip"`
	Duration  time.Duration `json:"duration,omitempty"`
	Reason    string        `json:"reason,omitempty"`
}

// Snapshot is the structure for the state snapshot file.
type Snapshot struct {
	Timestamp    time.Time                  `json:"snapshot_time"`
	ActiveBlocks map[string]ActiveBlockInfo `json:"active_blocks"`
}

// ActiveBlockInfo holds information about a currently active block.
type ActiveBlockInfo struct {
	UnblockTime time.Time `json:"unblock_time"`
	Reason      string    `json:"reason"`
}
