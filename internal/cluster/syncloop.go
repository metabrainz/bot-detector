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
					// Merge reasons, keep longest expiry
					merged[ip] = persistence.IPState{
						Reason:     MergeReasons(existing.Reason, state.Reason),
						ExpireTime: maxTime(existing.ExpireTime, state.ExpireTime),
					}
				} else {
					merged[ip] = state
				}
			}
		}
	}

	// Cache the merged state
	m.mergedStateCache.mu.Lock()
	m.mergedStateCache.state = merged
	m.mergedStateCache.ts = now
	m.mergedStateCache.mu.Unlock()

	m.log(logging.LevelDebug, "STATE_SYNC", "Cached merged state: %d IPs", len(merged))
}

// fetchAndMergeFromLeader fetches merged state from leader and merges with local
func (m *StateSyncManager) fetchAndMergeFromLeader() {
	leaderNode := m.findLeaderNode()
	if leaderNode == nil {
		m.log(logging.LevelWarning, "STATE_SYNC", "No leader node found in configuration")
		return
	}

	url := fmt.Sprintf("%s://%s/api/v1/cluster/state/merged", m.config.Protocol, leaderNode.Address)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		m.log(logging.LevelError, "STATE_SYNC", "Failed to create request: %v", err)
		return
	}

	if m.config.StateSync.Compression {
		req.Header.Set("Accept-Encoding", "gzip")
	}

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

	// Merge with local state
	now := time.Now()
	m.ipStatesMutex.Lock()
	for ip, state := range mergedResp.States {
		if state.ExpireTime.After(now) {
			if existing, ok := m.ipStates[ip]; ok {
				// Merge reasons, keep longest expiry
				m.ipStates[ip] = persistence.IPState{
					Reason:     MergeReasons(existing.Reason, state.Reason),
					ExpireTime: maxTime(existing.ExpireTime, state.ExpireTime),
				}
			} else {
				m.ipStates[ip] = state
			}
		}
	}
	m.ipStatesMutex.Unlock()

	m.log(logging.LevelDebug, "STATE_SYNC", "Merged %d IPs from leader", len(mergedResp.States))
}

// fetchNodeState fetches state from a specific node
func (m *StateSyncManager) fetchNodeState(node NodeConfig) (map[string]persistence.IPState, error) {
	url := fmt.Sprintf("%s://%s/api/v1/cluster/internal/persistence/state", m.config.Protocol, node.Address)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	if m.config.StateSync.Compression {
		req.Header.Set("Accept-Encoding", "gzip")
	}

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

	// Add source node to all reasons
	result := make(map[string]persistence.IPState)
	for ip, state := range stateResp.States {
		result[ip] = persistence.IPState{
			Reason:     AddSourceNode(state.Reason, node.Name, node.Address),
			ExpireTime: state.ExpireTime,
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

// GetMergedStateCache returns the cached merged state (for leader endpoint)
func (m *StateSyncManager) GetMergedStateCache() (map[string]persistence.IPState, time.Time) {
	m.mergedStateCache.mu.RLock()
	defer m.mergedStateCache.mu.RUnlock()
	return m.mergedStateCache.state, m.mergedStateCache.ts
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
