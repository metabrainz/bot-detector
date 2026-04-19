package cluster

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
)

// countingReader wraps a reader and counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// StateSyncManager manages periodic state synchronization
type StateSyncManager struct {
	config           *ClusterConfig
	role             string
	nodeName         string
	nodeAddress      string
	db               *sql.DB
	dbMutex          *sync.Mutex
	log              func(level logging.LogLevel, tag string, format string, args ...interface{})
	httpClient       *http.Client
	mergedStateCache *MergedStateCache
	lastSyncTime     time.Time // For incremental sync
	lastSyncMutex    sync.RWMutex
	// BadActorApplyFunc is called when a new bad actor is received from a peer.
	// It should insert into the local DB and issue a block to HAProxy.
	// Set by the application layer after creating the manager.
	BadActorApplyFunc func(ip string, score float64, blockCount int, promotedAt time.Time) error
}

// FetchMetrics contains metrics about a state fetch operation
type FetchMetrics struct {
	Compressed bool
	SizeKB     float64
	RateKBps   float64
	Duration   time.Duration
}

// FetchMergedState fetches merged state from a URL and returns the states, bad actors, timestamp, and metrics.
// This is shared between initial cluster fetch and periodic sync.
func FetchMergedState(url string, client *http.Client, requestCompression bool) (
	states map[string]persistence.IPState,
	badActors []persistence.BadActorInfo,
	timestamp time.Time,
	metrics FetchMetrics,
	err error,
) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, nil, time.Time{}, FetchMetrics{}, fmt.Errorf("failed to create request: %w", err)
	}

	if requestCompression {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	startTime := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, time.Time{}, FetchMetrics{}, fmt.Errorf("failed to fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, time.Time{}, FetchMetrics{}, fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	compressed := resp.Header.Get("Content-Encoding") == "gzip"
	if compressed {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, nil, time.Time{}, FetchMetrics{}, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	// Use a counting reader to track bytes read for metrics
	cr := &countingReader{r: reader}

	var mergedResp struct {
		Version   string                         `json:"version"`
		Timestamp time.Time                      `json:"timestamp"`
		States    map[string]persistence.IPState `json:"states"`
		BadActors []persistence.BadActorInfo     `json:"bad_actors,omitempty"`
	}

	if err := json.NewDecoder(cr).Decode(&mergedResp); err != nil {
		return nil, nil, time.Time{}, FetchMetrics{}, fmt.Errorf("failed to decode: %w", err)
	}

	duration := time.Since(startTime)
	sizeKB := float64(cr.n) / 1024.0
	var rateKBps float64
	if duration.Seconds() > 0 {
		rateKBps = sizeKB / duration.Seconds()
	}

	m := FetchMetrics{
		Compressed: compressed,
		SizeKB:     sizeKB,
		RateKBps:   rateKBps,
		Duration:   duration,
	}

	return mergedResp.States, mergedResp.BadActors, mergedResp.Timestamp, m, nil
}

// SetLastSyncTime sets the last sync timestamp (used after initial cluster fetch)
func (m *StateSyncManager) SetLastSyncTime(t time.Time) {
	m.lastSyncMutex.Lock()
	m.lastSyncTime = t
	m.lastSyncMutex.Unlock()
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
	db *sql.DB,
	dbMutex *sync.Mutex,
	logFunc func(level logging.LogLevel, tag string, format string, args ...interface{}),
) *StateSyncManager {
	return &StateSyncManager{
		config:      config,
		role:        role,
		nodeName:    nodeName,
		nodeAddress: nodeAddress,
		db:          db,
		dbMutex:     dbMutex,
		log:         logFunc,
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

	// Track stats
	localCount := 0
	remoteCount := 0
	remoteBadActorCount := 0
	nodesSucceeded := 0
	nodesFailed := 0

	// Add local state
	m.dbMutex.Lock()
	localStates, err := persistence.GetAllIPStates(m.db)
	m.dbMutex.Unlock()
	if err != nil {
		m.log(logging.LevelWarning, "STATE_SYNC", "Failed to query local states: %v", err)
	} else {
		for ip, state := range localStates {
			if state.State == persistence.BlockStateBlocked && state.ExpireTime.After(now) {
				merged[ip] = persistence.IPState{
					State:          state.State,
					Reason:         AddSourceNode(state.Reason, m.nodeName, m.nodeAddress),
					ExpireTime:     state.ExpireTime,
					ModifiedAt:     state.ModifiedAt,
					FirstBlockedAt: state.FirstBlockedAt,
				}
				localCount++
			} else if state.State == persistence.BlockStateUnblocked {
				merged[ip] = persistence.IPState{
					State:          state.State,
					Reason:         AddSourceNode(state.Reason, m.nodeName, m.nodeAddress),
					ExpireTime:     state.ExpireTime,
					ModifiedAt:     state.ModifiedAt,
					FirstBlockedAt: state.FirstBlockedAt,
				}
				localCount++
			}
		}
	}

	// Collect from other nodes
	for _, node := range m.config.Nodes {
		if node.Name == m.nodeName {
			continue
		}

		nodeState, nodeBadActors, err := m.fetchNodeState(node)
		if err != nil {
			m.log(logging.LevelWarning, "STATE_SYNC", "Failed to fetch state from %s: %v", node.Name, err)
			nodesFailed++
			continue
		}
		nodesSucceeded++

		// Apply bad actors from peer
		if m.BadActorApplyFunc != nil {
			for _, ba := range nodeBadActors {
				if err := m.BadActorApplyFunc(ba.IP, ba.TotalScore, ba.BlockCount, ba.PromotedAt); err != nil {
					m.log(logging.LevelWarning, "STATE_SYNC", "Failed to apply bad actor %s from %s: %v", ba.IP, node.Name, err)
				}
			}
		}
		remoteBadActorCount += len(nodeBadActors)

		// Merge node state
		nodeIPCount := 0
		for ip, state := range nodeState {
			if state.ExpireTime.After(now) {
				nodeIPCount++
				if existing, ok := merged[ip]; ok {
					// Merge reasons, keep longest expiry, latest modification, earliest block
					// Keep State from entry with longest expiry
					mergedState := persistence.IPState{
						Reason:         MergeReasons(existing.Reason, state.Reason),
						ExpireTime:     maxTime(existing.ExpireTime, state.ExpireTime),
						ModifiedAt:     maxTime(existing.ModifiedAt, state.ModifiedAt),
						FirstBlockedAt: minTime(existing.FirstBlockedAt, state.FirstBlockedAt),
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
					remoteCount++
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

	compressionMode := "plain"
	if m.config.StateSync.Compression {
		compressionMode = "gz"
	}

	// Count blocked vs unblocked
	blockedCount := 0
	unblockedCount := 0
	for _, state := range merged {
		if state.State == persistence.BlockStateBlocked {
			blockedCount++
		} else {
			unblockedCount++
		}
	}

	m.log(logging.LevelInfo, "STATE_SYNC", "Cached merged state: %d IPs (%d blocked + %d unblocked), local: %d, remote: %d, bad_actors: %d, nodes: %d/%d, mode: %s",
		len(merged), blockedCount, unblockedCount, localCount, remoteCount, remoteBadActorCount, nodesSucceeded+1, nodesSucceeded+nodesFailed+1, compressionMode)
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
	isIncremental := false
	if m.config.StateSync.Incremental {
		m.lastSyncMutex.RLock()
		if !m.lastSyncTime.IsZero() {
			url += "?since=" + m.lastSyncTime.UTC().Format(time.RFC3339)
			isIncremental = true
		}
		m.lastSyncMutex.RUnlock()
	}

	// Fetch using shared helper
	states, peerBadActors, responseTimestamp, metrics, err := FetchMergedState(url, m.httpClient, true)
	if err != nil {
		m.log(logging.LevelWarning, "STATE_SYNC", "Failed to fetch from leader: %v", err)
		return
	}

	// Apply bad actors from leader
	if m.BadActorApplyFunc != nil {
		for _, ba := range peerBadActors {
			if err := m.BadActorApplyFunc(ba.IP, ba.TotalScore, ba.BlockCount, ba.PromotedAt); err != nil {
				m.log(logging.LevelWarning, "STATE_SYNC", "Failed to apply bad actor %s from leader: %v", ba.IP, err)
			}
		}
	}

	// Update last sync time to the response timestamp (not current time)
	// This ensures the next incremental sync asks for changes after this response
	m.lastSyncMutex.Lock()
	m.lastSyncTime = responseTimestamp
	m.lastSyncMutex.Unlock()

	// Merge with local state using a single transaction for performance
	now := time.Now()
	m.dbMutex.Lock()
	const batchSize = 10000
	count := 0

	beginBatch := func() (*sql.Tx, *sql.Stmt, error) {
		tx, err := m.db.Begin()
		if err != nil {
			return nil, nil, err
		}
		stmt, err := tx.Prepare(`INSERT INTO ips (ip, state, expire_time, reason_id, modified_at, first_blocked_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(ip) DO UPDATE SET
				state = excluded.state,
				expire_time = MAX(ips.expire_time, excluded.expire_time),
				reason_id = excluded.reason_id,
				modified_at = MAX(ips.modified_at, excluded.modified_at),
				first_blocked_at = CASE
					WHEN ips.first_blocked_at IS NOT NULL AND ips.first_blocked_at > 0 AND ips.first_blocked_at < excluded.first_blocked_at
					THEN ips.first_blocked_at
					ELSE excluded.first_blocked_at
				END`)
		if err != nil {
			_ = tx.Rollback()
			return nil, nil, err
		}
		return tx, stmt, nil
	}

	tx, upsertStmt, err := beginBatch()
	if err != nil {
		m.dbMutex.Unlock()
		m.log(logging.LevelWarning, "STATE_SYNC", "Failed to begin transaction: %v", err)
		return
	}

	for ip, state := range states {
		if state.ExpireTime.After(now) {
			reasonID := persistence.ReasonHash(state.Reason)
			_, _ = tx.Exec("INSERT OR IGNORE INTO reasons (id, reason) VALUES (?, ?)", reasonID, state.Reason)
			_, _ = upsertStmt.Exec(ip, state.State.String(), persistence.TimeToUnix(state.ExpireTime), reasonID, persistence.TimeToUnix(state.ModifiedAt), persistence.TimeToUnix(state.FirstBlockedAt))
			count++

			if count%batchSize == 0 {
				_ = upsertStmt.Close()
				if err := tx.Commit(); err != nil {
					m.log(logging.LevelWarning, "STATE_SYNC", "Failed to commit batch: %v", err)
				}
				tx, upsertStmt, err = beginBatch()
				if err != nil {
					m.dbMutex.Unlock()
					m.log(logging.LevelWarning, "STATE_SYNC", "Failed to begin batch: %v", err)
					return
				}
			}
		}
	}

	_ = upsertStmt.Close()
	if err := tx.Commit(); err != nil {
		m.log(logging.LevelWarning, "STATE_SYNC", "Failed to commit final batch: %v", err)
	}
	m.dbMutex.Unlock()

	// Count blocked vs unblocked in received states
	blockedCount := 0
	unblockedCount := 0
	for _, state := range states {
		if state.State == persistence.BlockStateBlocked {
			blockedCount++
		} else {
			unblockedCount++
		}
	}

	// Format sync mode for logging
	modeStr := "gz,full"
	if !metrics.Compressed {
		modeStr = "plain,full"
	}
	deltaStr := ""
	if isIncremental {
		deltaStr = fmt.Sprintf(", delta: %d", len(states))
		if metrics.Compressed {
			modeStr = "gz,incr"
		} else {
			modeStr = "plain,incr"
		}
	}

	m.log(logging.LevelInfo, "STATE_SYNC", "Merged from leader: %d IPs (%d blocked + %d unblocked%s), %d bad actors, size: %.1f KB, rate: %.1f KB/s, duration: %v, mode: %s",
		len(states), blockedCount, unblockedCount, deltaStr, len(peerBadActors), metrics.SizeKB, metrics.RateKBps, metrics.Duration.Round(time.Millisecond), modeStr)
}

// fetchNodeState fetches state from a specific node
func (m *StateSyncManager) fetchNodeState(node NodeConfig) (map[string]persistence.IPState, []persistence.BadActorInfo, error) {
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
		return nil, nil, err
	}

	// Always request gzip, let server decide
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	var stateResp struct {
		Version   string                         `json:"version"`
		States    map[string]persistence.IPState `json:"states"`
		BadActors []persistence.BadActorInfo     `json:"bad_actors,omitempty"`
	}

	if err := json.NewDecoder(reader).Decode(&stateResp); err != nil {
		return nil, nil, err
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

	return result, stateResp.BadActors, nil
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

func minTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.Before(b) {
		return a
	}
	return b
}
