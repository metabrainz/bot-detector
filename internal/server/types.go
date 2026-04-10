package server

import (
	"os"
	"sync"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
)

// NodeStatus represents the cluster status of this node.
// This is used by the HTTP server to return node identity information.
type NodeStatus struct {
	Role          string // "leader" or "follower"
	Name          string // Node name from cluster config (empty if not configured)
	Address       string // Node address from cluster config (empty if not configured)
	LeaderAddress string // Leader address (only set for followers)
}

// Provider defines the interface required by the HTTP server to access application data.
// This interface decouples the server from the main application implementation,
// allowing the server to request metrics, configuration, and lifecycle information
// without direct dependencies on internal application structures.
type Provider interface {
	// GetListenConfigs returns all configured listen addresses.
	// Returns interface{} to avoid import cycle (actual type: []*commandline.ListenConfig).
	GetListenConfigs() interface{}

	// GetShutdownChannel returns a channel that signals when the application is shutting down.
	GetShutdownChannel() chan os.Signal

	// Log writes a log message with the specified level, tag, and format.
	Log(level logging.LogLevel, tag string, format string, v ...interface{})

	// GetConfigForArchive retrieves the main configuration and all file dependencies
	// for creating a configuration archive.
	GetConfigForArchive() (mainConfig []byte, modTime time.Time, deps map[string]*types.FileDependency, configDir string, err error)

	// GenerateMetricsReport generates a plain-text metrics report.
	GenerateMetricsReport() string

	// GenerateStepsMetricsReport generates a plain-text step execution metrics report.
	GenerateStepsMetricsReport() string

	// GenerateWebsiteStatsReport generates a plain-text multi-website statistics report.
	GenerateWebsiteStatsReport() string

	// GetMarshalledConfig retrieves the raw YAML configuration bytes and its modification time.
	GetMarshalledConfig() ([]byte, time.Time, error)

	// GetNodeStatus returns the cluster status of this node (role, name, address, leader).
	GetNodeStatus() NodeStatus

	// GetMetricsSnapshot returns a JSON-serializable snapshot of current metrics.
	GetMetricsSnapshot() MetricsSnapshot

	// GetAggregatedMetrics returns cluster-wide aggregated metrics (leader only).
	// Returns nil if this node is not a leader or if cluster is not configured.
	GetAggregatedMetrics() interface{}

	// GetActivityStore returns the actor activity map for IP lookup.
	GetActivityStore() map[store.Actor]*store.ActorActivity

	// GetActivityMutex returns the mutex for ActivityStore.
	GetActivityMutex() *sync.RWMutex

	// GetNodeName returns the cluster node name (empty if not in cluster).
	GetNodeName() string

	// GetNodeRole returns "leader", "follower", or empty string.
	GetNodeRole() string

	// GetNodeLeaderAddress returns leader address (for followers).
	GetNodeLeaderAddress() string

	// GetClusterNodes returns list of cluster nodes (nil if not in cluster).
	// Returns []cluster.NodeConfig but typed as interface{} to avoid import cycle.
	GetClusterNodes() interface{}

	// GetClusterProtocol returns cluster protocol ("http" or "https").
	GetClusterProtocol() string

	// GetBlocker returns the blocker instance for IP removal operations.
	GetBlocker() interface{}

	// GetDurationTables returns the configured duration-to-table mappings.
	GetDurationTables() map[time.Duration]string

	// GetPersistenceState returns the persistence state for an IP (if exists).
	GetPersistenceState(ip string) (interface{}, bool)

	// RemoveFromPersistence removes an IP from persistence state and writes unblock event to journal.
	RemoveFromPersistence(ip string) error

	// GetIPStates returns the complete IPStates map for state sync.
	GetIPStates() map[string]persistence.IPState

	// GetPersistenceMutex returns the mutex for IPStates.
	GetPersistenceMutex() *sync.Mutex

	// GetClusterConfig returns the cluster configuration (nil if not in cluster).
	// Returns *cluster.ClusterConfig but typed as interface{} to avoid import cycle.
	GetClusterConfig() interface{}

	// GetStateSyncConfig returns state sync configuration values.
	// Returns (enabled, compression, timeout, incremental).
	GetStateSyncConfig() (bool, bool, time.Duration, bool)

	// GetStateSyncManager returns the state sync manager (nil if not enabled).
	// Returns *cluster.StateSyncManager but typed as interface{} to avoid import cycle.
	GetStateSyncManager() interface{}

	// GetBadActorInfo returns bad actor info and score for an IP.
	// Returns (badActorInfo, scoreInfo) — either can be nil.
	GetBadActorInfo(ip string) (interface{}, interface{})

	// GetAllBadActors returns all bad actors.
	GetAllBadActors() ([]interface{}, error)

	// RemoveBadActorsByReason removes bad actors whose history contains the given reason.
	// Returns the list of removed IPs.
	RemoveBadActorsByReason(reason string) ([]string, error)

	// GetBlockedIPsByReason returns IPs currently blocked with a reason containing the given substring.
	GetBlockedIPsByReason(reason string) ([]string, error)

	// GetBadActorsThreshold returns the configured bad actor threshold (0 if disabled).
	GetBadActorsThreshold() float64
}

// StateSyncResponse is the response format for state sync endpoints.
type StateSyncResponse struct {
	Version   string                         `json:"version"`
	Timestamp time.Time                      `json:"timestamp"`
	States    map[string]persistence.IPState `json:"states"`
	BadActors []persistence.BadActorInfo     `json:"bad_actors,omitempty"`
}

// MergedStateResponse is the response format for merged cluster state.
type MergedStateResponse struct {
	Version      string                         `json:"version"`
	Timestamp    time.Time                      `json:"timestamp"`
	NodesQueried []string                       `json:"nodes_queried"`
	NodesFailed  []string                       `json:"nodes_failed"`
	States       map[string]persistence.IPState `json:"states"`
	BadActors    []persistence.BadActorInfo     `json:"bad_actors,omitempty"`
}
