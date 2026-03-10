package cluster

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
)

// StateSyncManager manages periodic state synchronization
type StateSyncManager struct {
	config           *ClusterConfig
	role             string
	nodeName         string
	nodeAddress      string
	ipStates         map[string]persistence.IPState
	ipStatesMutex    *sync.Mutex
	log              func(level logging.LogLevel, tag string, format string, args ...interface{})
	httpClient       *http.Client
	mergedStateCache *MergedStateCache
	lastSyncTime     time.Time // For incremental sync
	lastSyncMutex    sync.RWMutex
}

// MergedStateCache stores the leader's merged state
type MergedStateCache struct {
	mu    sync.RWMutex
	state map[string]persistence.IPState
	ts    time.Time
}

// NewStateSyncManager creates a new state sync manager
func NewStateSyncManager(
	config *ClusterConfig,
	role string,
	nodeName string,
	nodeAddress string,
	ipStates map[string]persistence.IPState,
	ipStatesMutex *sync.Mutex,
	logFunc func(level logging.LogLevel, tag string, format string, args ...interface{}),
) *StateSyncManager {
	return &StateSyncManager{
		config:        config,
		role:          role,
		nodeName:      nodeName,
		nodeAddress:   nodeAddress,
		ipStates:      ipStates,
		ipStatesMutex: ipStatesMutex,
		log:           logFunc,
		httpClient: &http.Client{
			Timeout: config.StateSync.Timeout,
		},
		mergedStateCache: &MergedStateCache{
			state: make(map[string]persistence.IPState),
		},
	}
}

// Start begins the sync loop based on node role
func (m *StateSyncManager) Start(ctx context.Context) {
	if !m.config.StateSync.Enabled {
		m.log(logging.LevelInfo, "STATE_SYNC", "State synchronization disabled")
		return
	}

	if m.role == "leader" {
		go m.leaderSyncLoop(ctx)
	} else {
		go m.followerSyncLoop(ctx)
	}
}

// leaderSyncLoop periodically collects and merges states from all nodes
func (m *StateSyncManager) leaderSyncLoop(ctx context.Context) {
	m.log(logging.LevelInfo, "STATE_SYNC", "Starting leader sync loop (interval: %v)", m.config.StateSync.Interval)
	ticker := time.NewTicker(m.config.StateSync.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log(logging.LevelInfo, "STATE_SYNC", "Leader sync loop stopped")
			return
		case <-ticker.C:
			m.collectAndCacheMergedState()
		}
	}
}

// followerSyncLoop periodically fetches merged state from leader
func (m *StateSyncManager) followerSyncLoop(ctx context.Context) {
	m.log(logging.LevelInfo, "STATE_SYNC", "Starting follower sync loop (interval: %v)", m.config.StateSync.Interval)
	ticker := time.NewTicker(m.config.StateSync.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log(logging.LevelInfo, "STATE_SYNC", "Follower sync loop stopped")
			return
		case <-ticker.C:
			m.fetchAndMergeFromLeader()
		}
	}
}

// collectAndCacheMergedState collects states from all nodes and caches the result
func (m *StateSyncManager) collectAndCacheMergedState() {
	merged := make(map[string]persistence.IPState)
	now := time.Now()

	// Add local state
	m.ipStatesMutex.Lock()
	for ip, state := range m.ipStates {
		if state.ExpireTime.After(now) {
			merged[ip] = persistence.IPState{
				Reason:     AddSourceNode(state.Reason, m.nodeName, m.nodeAddress),
				ExpireTime: state.ExpireTime,
				ModifiedAt: state.ModifiedAt,
			}
		}
	}
	m.ipStatesMutex.Unlock()

	// Collect from other nodes
	for _, node := range m.config.Nodes {
		if node.Name == m.nodeName {
			continue
		}

		nodeState, err := m.fetchNodeState(node)
		if err != nil {
			m.log(logging.LevelWarning, "STATE_SYNC", "Failed to fetch state from %s: %v", node.Name, err)
			continue
		}

		// Merge node state
		for ip, state := range nodeState {
			if state.ExpireTime.After(now) {
				if existing, ok := merged[ip]; ok {
					// Merge reasons, keep longest expiry, latest modification
					// Keep State from entry with longest expiry
					mergedState := persistence.IPState{
						Reason:     MergeReasons(existing.Reason, state.Reason),
						ExpireTime: maxTime(existing.ExpireTime, state.ExpireTime),
						ModifiedAt: maxTime(existing.ModifiedAt, state.ModifiedAt),
					}
					// Use State from the entry with longest expiry
					if state.ExpireTime.After(existing.ExpireTime) {
						mergedState.State = state.State
					} else {
						mergedState.State = existing.State
					}
					merged[ip] = mergedState
				} else {
					merged[ip] = state
				}
			}
		}
	}

	// Update last sync time
	m.lastSyncMutex.Lock()
	m.lastSyncTime = now
	m.lastSyncMutex.Unlock()

	// Cache the merged state
	m.mergedStateCache.mu.Lock()
	m.mergedStateCache.state = merged
	m.mergedStateCache.ts = now
	m.mergedStateCache.mu.Unlock()

	m.log(logging.LevelInfo, "STATE_SYNC", "Cached merged state: %d IPs", len(merged))
}

// fetchAndMergeFromLeader fetches merged state from leader and merges with local
func (m *StateSyncManager) fetchAndMergeFromLeader() {
	leaderNode := m.findLeaderNode()
	if leaderNode == nil {
		m.log(logging.LevelWarning, "STATE_SYNC", "No leader node found in configuration")
		return
	}

	url := fmt.Sprintf("%s://%s/api/v1/cluster/state/merged", m.config.Protocol, leaderNode.Address)

	// Add since parameter for incremental sync
	if m.config.StateSync.Incremental {
		m.lastSyncMutex.RLock()
		if !m.lastSyncTime.IsZero() {
			url += "?since=" + m.lastSyncTime.UTC().Format(time.RFC3339)
		}
		m.lastSyncMutex.RUnlock()
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		m.log(logging.LevelError, "STATE_SYNC", "Failed to create request: %v", err)
		return
	}

	// Always request gzip, let server decide
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		m.log(logging.LevelWarning, "STATE_SYNC", "Failed to fetch from leader: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		m.log(logging.LevelWarning, "STATE_SYNC", "Leader returned status %d", resp.StatusCode)
		return
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			m.log(logging.LevelError, "STATE_SYNC", "Failed to create gzip reader: %v", err)
			return
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	var mergedResp struct {
		Version string                         `json:"version"`
		States  map[string]persistence.IPState `json:"states"`
	}

	if err := json.NewDecoder(reader).Decode(&mergedResp); err != nil {
		m.log(logging.LevelError, "STATE_SYNC", "Failed to decode merged state: %v", err)
		return
	}

	// Check version compatibility
	if mergedResp.Version != StateSyncVersion {
		m.log(logging.LevelWarning, "STATE_SYNC", "Version mismatch from leader: got %s, expected %s", mergedResp.Version, StateSyncVersion)
		// Continue anyway for backward compatibility
	}

	// Update last sync time
	m.lastSyncMutex.Lock()
	m.lastSyncTime = time.Now()
	m.lastSyncMutex.Unlock()

	// Merge with local state
	now := time.Now()
	m.ipStatesMutex.Lock()
	for ip, state := range mergedResp.States {
		if state.ExpireTime.After(now) {
			if existing, ok := m.ipStates[ip]; ok {
				// Merge reasons, keep longest expiry, latest modification
				mergedState := persistence.IPState{
					Reason:     MergeReasons(existing.Reason, state.Reason),
					ExpireTime: maxTime(existing.ExpireTime, state.ExpireTime),
					ModifiedAt: maxTime(existing.ModifiedAt, state.ModifiedAt),
				}
				// Use State from the entry with longest expiry
				if state.ExpireTime.After(existing.ExpireTime) {
					mergedState.State = state.State
				} else {
					mergedState.State = existing.State
				}
				m.ipStates[ip] = mergedState
			} else {
				m.ipStates[ip] = state
			}
		}
	}
	m.ipStatesMutex.Unlock()

	m.log(logging.LevelInfo, "STATE_SYNC", "Merged %d IPs from leader", len(mergedResp.States))
}

// fetchNodeState fetches state from a specific node
func (m *StateSyncManager) fetchNodeState(node NodeConfig) (map[string]persistence.IPState, error) {
	url := fmt.Sprintf("%s://%s/api/v1/cluster/internal/persistence/state", m.config.Protocol, node.Address)

	// Add since parameter for incremental sync
	if m.config.StateSync.Incremental {
		m.lastSyncMutex.RLock()
		if !m.lastSyncTime.IsZero() {
			// Use URL query parameter properly
			url += "?since=" + m.lastSyncTime.UTC().Format(time.RFC3339)
		}
		m.lastSyncMutex.RUnlock()
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Always request gzip, let server decide
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	var stateResp struct {
		Version string                         `json:"version"`
		States  map[string]persistence.IPState `json:"states"`
	}

	if err := json.NewDecoder(reader).Decode(&stateResp); err != nil {
		return nil, err
	}

	// Check version compatibility
	if stateResp.Version != StateSyncVersion {
		m.log(logging.LevelWarning, "STATE_SYNC", "Version mismatch from %s: got %s, expected %s", node.Name, stateResp.Version, StateSyncVersion)
		// Continue anyway for backward compatibility
	}

	// Add source node to all reasons
	result := make(map[string]persistence.IPState)
	for ip, state := range stateResp.States {
		result[ip] = persistence.IPState{
			Reason:     AddSourceNode(state.Reason, node.Name, node.Address),
			ExpireTime: state.ExpireTime,
			ModifiedAt: state.ModifiedAt,
		}
	}

	return result, nil
}

// findLeaderNode finds the leader node in the configuration
func (m *StateSyncManager) findLeaderNode() *NodeConfig {
	for i := range m.config.Nodes {
		if m.config.Nodes[i].Name != m.nodeName {
			// First non-self node is assumed to be leader
			return &m.config.Nodes[i]
		}
	}
	return nil
}

// GetMergedStateCache returns a copy of the cached merged state (for leader endpoint)
func (m *StateSyncManager) GetMergedStateCache() (map[string]persistence.IPState, time.Time) {
	m.mergedStateCache.mu.RLock()
	defer m.mergedStateCache.mu.RUnlock()

	// Return a copy to prevent external modification
	stateCopy := make(map[string]persistence.IPState, len(m.mergedStateCache.state))
	for k, v := range m.mergedStateCache.state {
		stateCopy[k] = v
	}

	return stateCopy, m.mergedStateCache.ts
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
