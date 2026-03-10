package cluster

import (
	"time"

	"bot-detector/internal/persistence"
)

const (
	// StateSyncVersion is the current state sync protocol version.
	StateSyncVersion = "v1"
)

// StateSyncResponse is the response format for state sync endpoints.
type StateSyncResponse struct {
	Version   string                         `json:"version"`
	Timestamp time.Time                      `json:"timestamp"`
	States    map[string]persistence.IPState `json:"states"`
}

// MergedStateResponse is the response format for merged cluster state.
type MergedStateResponse struct {
	Version      string                         `json:"version"`
	Timestamp    time.Time                      `json:"timestamp"`
	NodesQueried []string                       `json:"nodes_queried"`
	NodesFailed  []string                       `json:"nodes_failed"`
	States       map[string]persistence.IPState `json:"states"`
}

// StateSyncConfig holds configuration for state synchronization.
type StateSyncConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Interval    time.Duration `yaml:"interval"`
	Compression bool          `yaml:"compression"`
	Timeout     time.Duration `yaml:"timeout"`
	Incremental bool          `yaml:"incremental"`
}
