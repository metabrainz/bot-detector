# Cluster State Synchronization

## Status

**Implementation:** ✅ Complete  
**Version:** v1  
**Commits:** 70dfef4, 9f336bd, aaa1728, 740e442, a026c6e, 0b1f0a7

## Overview

In a multi-node cluster, each node processes logs independently and blocks IPs based on its own detection rules. To ensure all nodes have visibility into blocks created by other nodes (for IP lookup queries and persistence), the cluster implements **bidirectional state synchronization**.

This document describes the persistence-based state sync mechanism that allows nodes to share their block reasons without requiring shared storage or external dependencies.

## Problem Statement

### The Challenge

**Scenario:**
1. Node A (leader) blocks IP `1.2.3.4` for `Login-Abuse[main_site]`
2. IP is written to HAProxy stick tables (shared across nodes)
3. User queries Node B (follower) for IP `1.2.3.4` status
4. Node B sees IP in HAProxy but has no record of why it was blocked

**Result:** Block reason is unavailable on Node B.

### Why This Matters

- **Operational visibility:** Admins need to know why IPs are blocked
- **Audit trail:** Security compliance requires block reasons
- **Debugging:** Understanding detection patterns across cluster
- **Persistence:** If Node A goes down, Node B should still have the reason

### Scale Considerations

- **Typical deployment:** 250K+ blocked IPs
- **Block rate:** 10-100 blocks/minute (varies by traffic)
- **Cluster size:** 2 nodes (leader + follower)
- **Persistence size:** ~25 MB for 250K IPs

## Solution: Persistence-Based State Sync

### Architecture

**Leader (Active Role):**
1. Periodically queries all followers for their `IPStates` (persistence state)
2. Merges all states including its own → creates cluster-wide view
3. Exposes merged state via dedicated endpoint

**Followers (Passive Role):**
1. Periodically query leader for merged cluster state
2. Merge received state with their own local state
3. Result: All nodes have complete cluster-wide block reasons

### Data Flow

```
┌─────────────────────────────────────────────────────────────┐
│                         LEADER                              │
│                                                             │
│  1. Query followers for IPStates                            │
│     GET /api/v1/cluster/internal/persistence/state         │
│                                                             │
│  2. Merge all states (with conflict resolution)             │
│     - Combine reasons from different nodes                  │
│     - Keep longest expiry time                              │
│     - Track source node for each reason                     │
│                                                             │
│  3. Expose merged state                                     │
│     GET /api/v1/cluster/state/merged                        │
└─────────────────────────────────────────────────────────────┘
                            │
                            │ Periodic sync
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                       FOLLOWER                              │
│                                                             │
│  1. Query leader for merged state                           │
│     GET /api/v1/cluster/state/merged                        │
│                                                             │
│  2. Merge with local IPStates                               │
│     - Add new IPs                                           │
│     - Update existing with newer/longer blocks              │
│     - Preserve local reasons                                │
└─────────────────────────────────────────────────────────────┘
```

## Implementation Approaches

### Option A: Full State Sync (Recommended for Initial Implementation)

**How it works:**
- Leader queries all followers for complete `IPStates` map
- Merges all states and exposes via endpoint
- Followers fetch complete merged state periodically

**Pros:**
- ✅ Simple implementation
- ✅ No change tracking needed
- ✅ Self-healing (always converges to correct state)
- ✅ Easy to debug

**Cons:**
- ⚠️ Higher bandwidth usage
- ⚠️ Transfers unchanged data

**Bandwidth cost (250K IPs):**
```
Uncompressed: 25 MB every 60s = 417 KB/s
With gzip:     2-5 MB every 60s = 33-83 KB/s
```

**Optimization: Compression**
```go
w.Header().Set("Content-Encoding", "gzip")
gz := gzip.NewWriter(w)
defer gz.Close()
json.NewEncoder(gz).Encode(states)
```

JSON compresses very well (80-90% reduction) due to repetitive structure.

---

### Option B: Incremental Sync (Future Optimization)

**How it works:**
- Track modification time for each IP state
- Clients send `since` timestamp with request
- Server returns only states modified after `since`

**Pros:**
- ✅ Minimal bandwidth (only changes)
- ✅ Scales to millions of IPs
- ✅ Efficient for steady-state operation

**Cons:**
- ⚠️ Requires modification time tracking
- ⚠️ More complex implementation
- ⚠️ Need full sync fallback for new nodes

**Bandwidth cost (100 blocks/min):**
```
Changes in 60s: 6000 IPs × 100 bytes = 600 KB
With gzip: ~60-120 KB every 60s = 1-2 KB/s
```

**Implementation:**
```go
// Track modification time per IP
type IPStateWithTime struct {
    persistence.IPState
    ModifiedAt time.Time `json:"modified_at"`
}

// Endpoint supports incremental sync
GET /api/v1/cluster/internal/persistence/state?since=2026-03-10T10:00:00Z
```

**When to implement:**
- Active blocks consistently >100K
- Network bandwidth becomes constrained
- Sync latency becomes noticeable

---

## Conflict Resolution

### Scenario: Same IP Blocked by Multiple Nodes

**Example:**
- Leader blocks `1.2.3.4` for `Login-Abuse[main_site]` at 10:00:00, expires 10:30:00
- Follower blocks `1.2.3.4` for `API-Abuse[api_site]` at 10:00:05, expires 10:15:00

### Resolution Strategy

**1. Merge Reasons (Preserve All Information)**

Combine reasons from different nodes with source attribution:

```go
// Before merge
Leader:   "Login-Abuse[main_site]"
Follower: "API-Abuse[api_site]"

// After merge (on all nodes)
"Login-Abuse[main_site] (leader), API-Abuse[api_site] (follower-1)"
```

**2. Use Longest Expiry**

Keep the block active until the longest expiry time:

```go
if existing.ExpireTime.After(state.ExpireTime) {
    state.ExpireTime = existing.ExpireTime
}
```

**3. Prevent Reason Duplication**

Track which reasons have been added to prevent loops:

```go
func mergeReasons(existing, new string, sourceNode string) string {
    // Parse existing reasons into map
    reasonMap := make(map[string]bool)
    for _, part := range strings.Split(existing, ", ") {
        // Extract reason without source node
        reason := strings.Split(part, " (")[0]
        reasonMap[reason] = true
    }
    
    // Extract new reason without source
    newReason := strings.Split(new, " (")[0]
    
    // Only add if not already present
    if !reasonMap[newReason] {
        if existing != "" {
            return fmt.Sprintf("%s, %s (%s)", existing, newReason, sourceNode)
        }
        return fmt.Sprintf("%s (%s)", newReason, sourceNode)
    }
    
    return existing
}
```

**Example with loop prevention:**
```
Sync 1: "Login-Abuse[main_site] (leader)"
Sync 2: "Login-Abuse[main_site] (leader), API-Abuse[api_site] (follower-1)"
Sync 3: "Login-Abuse[main_site] (leader), API-Abuse[api_site] (follower-1)"
        ↑ No duplication - same reasons not added again
```

---

## API Endpoints

### 1. Get Node's Persistence State

**Endpoint:** `GET /api/v1/cluster/internal/persistence/state`

**Available on:** All nodes (leader and followers)

**Purpose:** Expose node's local `IPStates` for leader to collect

**Query Parameters:**
- `since` (optional): RFC3339 timestamp - return only states modified after this time (incremental sync)

**Response:**
```json
{
  "timestamp": "2026-03-10T10:00:00Z",
  "states": {
    "1.2.3.4": {
      "state": "blocked",
      "expire_time": "2026-03-10T11:00:00Z",
      "reason": "Login-Abuse[main_site]"
    },
    "5.6.7.8": {
      "state": "blocked",
      "expire_time": "2026-03-10T10:30:00Z",
      "reason": "API-Rate-Limit[api_site]"
    }
  },
  "bad_actors": [
    {
      "ip": "9.8.7.6",
      "promoted_at": "2026-03-09T08:00:00Z",
      "total_score": 5.0,
      "block_count": 5
    }
  ]
}
```

**Compression:** Supports gzip compression (recommended)

---

### 2. Get Merged Cluster State

**Endpoint:** `GET /api/v1/cluster/state/merged`

**Available on:** Leader only

**Purpose:** Provide cluster-wide merged state to followers

**Query Parameters:**
- `since` (optional): RFC3339 timestamp - return only states modified after this time (incremental sync)

**Response:**
```json
{
  "timestamp": "2026-03-10T10:00:00Z",
  "nodes_queried": ["leader", "follower-1"],
  "nodes_failed": [],
  "states": {
    "1.2.3.4": {
      "state": "blocked",
      "expire_time": "2026-03-10T11:00:00Z",
      "reason": "Login-Abuse[main_site] (leader), API-Abuse[api_site] (follower-1)"
    }
  },
  "bad_actors": [
    {
      "ip": "9.8.7.6",
      "promoted_at": "2026-03-09T08:00:00Z",
      "total_score": 5.0,
      "block_count": 5
    }
  ]
}
```

**Error Handling:**
- If follower is unreachable, leader logs warning and continues with available states
- `nodes_failed` array lists nodes that couldn't be queried
- Partial state is still useful (better than nothing)

---

## Bad Actor Sync

Bad actors are included in the state sync protocol alongside IP states. Both the per-node persistence state endpoint and the leader's merged state endpoint include a `bad_actors` array containing all promoted bad actors.

### How It Works

1. Each node includes its local `bad_actors` table in the `GET /api/v1/cluster/internal/persistence/state` response
2. The leader collects bad actors from all peers during its periodic merge cycle
3. Followers receive bad actors from the leader's `GET /api/v1/cluster/state/merged` response
4. When a new bad actor is received, the node inserts it into its local database and issues a block to HAProxy
5. Already-known bad actors are silently skipped (idempotent)

### Failover Scenario

With IP-hash load balancing, each IP routes to a single node. If that node promotes an IP to bad actor, the promotion is synced to the other node within one sync interval. If the original node goes down, the surviving node already has the bad actor in its database and blocks it immediately when traffic shifts over.

### Backward Compatibility

The `bad_actors` field uses `omitempty` — nodes running older versions without bad actor sync will simply not include it in their response. Peers will decode the response normally and see an empty bad actors list. No errors, no special handling needed.

---

## Configuration

```yaml
cluster:
  # ... existing cluster config ...
  
  state_sync:
    enabled: true              # Enable state synchronization
    interval: "60s"            # How often to sync (leader queries followers, followers query leader)
    compression: true          # Use gzip compression for state transfer
    timeout: "30s"             # HTTP timeout for state queries
    incremental: true          # Use incremental sync (only changed IPs)
    batch_size: 10000          # Rows per DB transaction batch on follower (default: 10000)
```

**Requirements:**
- Persistence must be enabled (`--state-dir` flag)
- Cluster mode must be configured
- All nodes must have state sync enabled

**Recommended settings:**
- **Small clusters (<50K IPs):** `interval: 30s`, `compression: true`
- **Large clusters (>100K IPs):** `interval: 60s`, `compression: true`
- **High block rate:** Shorter interval (30s) for faster propagation
- **Low block rate:** Longer interval (120s) to reduce overhead

**Example:**
```yaml
cluster:
  nodes:
    - name: "leader"
      address: "leader.example.com:8080"
    - name: "follower-1"
      address: "follower1.example.com:8080"
  
  config_poll_interval: "30s"
  metrics_report_interval: "10s"
  protocol: "http"
  
  state_sync:
    enabled: true
    interval: "60s"
    compression: true
    timeout: "30s"
    incremental: false
```

---

## Implementation Details

### Components

**1. StateSyncManager** (`internal/cluster/syncloop.go`)
- Manages periodic sync loops for leader and follower
- Handles HTTP requests to other nodes
- Caches merged state (leader only)
- Implements conflict resolution

**2. HTTP Endpoints** (`internal/server/handlers_statesync.go`)
- `GET /api/v1/cluster/internal/persistence/state` - Node's local state
- `GET /api/v1/cluster/state/merged` - Cluster-wide merged state (leader only)

**3. Configuration** (`internal/config/`)
- YAML parsing for `state_sync` section
- Defaults: enabled=true, interval=60s, compression=true

**4. Integration** (`cmd/bot-detector/main.go`)
- Initialize StateSyncManager based on cluster config
- Start sync loop based on node role
- Context-based cancellation on shutdown

### Sync Loop Implementation

### Leader: Collect and Merge States

The leader periodically (every `interval`):
1. Collects its own IPStates
2. Queries all followers via HTTP
3. Merges states with conflict resolution
4. Caches result for followers to query

```go
func (m *StateSyncManager) collectAndCacheMergedState() {
    merged := make(map[string]persistence.IPState)
    
    // Add leader's own state with source attribution
    for ip, state := range m.ipStates {
        merged[ip] = persistence.IPState{
            Reason:     AddSourceNode(state.Reason, m.nodeName, m.nodeAddress),
            ExpireTime: state.ExpireTime,
        }
    }
    
    // Query each follower
    for _, node := range m.config.Nodes {
        if node.Name == m.nodeName {
            continue  // Skip self
        }
        
        url := fmt.Sprintf("%s://%s/api/v1/cluster/internal/persistence/state",
            p.Cluster.Protocol, node.Address)
        
        resp, err := http.Get(url)
        if err != nil {
            p.LogFunc(logging.LevelWarning, "STATE_MERGE", 
                "Failed to fetch state from %s: %v", node.Name, err)
            continue
        }
        defer resp.Body.Close()
        
        var followerStates map[string]persistence.IPState
        if err := json.NewDecoder(resp.Body).Decode(&followerStates); err != nil {
            p.LogFunc(logging.LevelWarning, "STATE_MERGE", 
                "Failed to decode state from %s: %v", node.Name, err)
            continue
        }
        
        // Merge with conflict resolution
        for ip, state := range followerStates {
            // Add source node to reason
            state.Reason = addSourceNode(state.Reason, node.Name)
            
            if existing, ok := merged[ip]; ok {
                // Merge reasons (prevent duplication)
                state.Reason = mergeReasons(existing.Reason, state.Reason, node.Name)
                
                // Keep longer expiry
                if existing.ExpireTime.After(state.ExpireTime) {
                    state.ExpireTime = existing.ExpireTime
                }
            }
            merged[ip] = state
        }
    }
    
    return merged
}

// Helper: Add source node to reason if not already present
func addSourceNode(reason, nodeName string) string {
    // Check if reason already has source attribution
    if strings.Contains(reason, " (") && strings.Contains(reason, ")") {
        return reason
    }
    return fmt.Sprintf("%s (%s)", reason, nodeName)
}

// Helper: Merge reasons without duplication
func mergeReasons(existing, new string, sourceNode string) string {
    // Parse existing reasons into map (reason -> true)
    reasonMap := make(map[string]bool)
    for _, part := range strings.Split(existing, ", ") {
        // Extract reason without source node: "Login-Abuse[main_site] (leader)" -> "Login-Abuse[main_site]"
        if idx := strings.Index(part, " ("); idx != -1 {
            reason := part[:idx]
            reasonMap[reason] = true
        } else {
            reasonMap[part] = true
        }
    }
    
    // Extract new reason without source
    newReason := new
    if idx := strings.Index(new, " ("); idx != -1 {
        newReason = new[:idx]
    }
    
    // Only add if not already present
    if !reasonMap[newReason] {
        if existing != "" {
            return fmt.Sprintf("%s, %s", existing, new)
        }
        return new
    }
    
    return existing
}
```

### Follower: Sync from Leader

```go
func (p *Processor) syncFromLeader() {
    leaderAddr := p.getLeaderAddress()
    if leaderAddr == "" {
        return
    }
    
    url := fmt.Sprintf("%s://%s/api/v1/cluster/state/merged",
        p.Cluster.Protocol, leaderAddr)
    
    resp, err := http.Get(url)
    if err != nil {
        p.LogFunc(logging.LevelWarning, "STATE_SYNC", 
            "Failed to fetch merged state from leader: %v", err)
        return
    }
    defer resp.Body.Close()
    
    var mergedStates map[string]persistence.IPState
    if err := json.NewDecoder(resp.Body).Decode(&mergedStates); err != nil {
        p.LogFunc(logging.LevelWarning, "STATE_SYNC", 
            "Failed to decode merged state: %v", err)
        return
    }
    
    // Merge with local state
    p.PersistenceMutex.Lock()
    updated := 0
    added := 0
    for ip, state := range mergedStates {
        if existing, ok := p.IPStates[ip]; ok {
            // Update if newer or longer expiry
            if state.ExpireTime.After(existing.ExpireTime) {
                p.IPStates[ip] = state
                updated++
            }
        } else {
            // New IP not in local state
            p.IPStates[ip] = state
            added++
        }
    }
    p.PersistenceMutex.Unlock()
    
    if added > 0 || updated > 0 {
        p.LogFunc(logging.LevelInfo, "STATE_SYNC", 
            "Synced from leader: %d new, %d updated (total: %d)", 
            added, updated, len(mergedStates))
    }
}
```

---

## Operational Considerations

### Startup Behavior

**New follower joining cluster:**
1. Starts with empty or outdated `IPStates`
2. First sync fetches complete cluster state from leader
3. Subsequent syncs maintain up-to-date state

**Leader restart:**
1. Loads state from local persistence
2. First sync collects states from all followers
3. Merges and exposes complete cluster view

### Network Partition

**Follower loses connection to leader:**
- Follower continues processing logs and blocking IPs locally
- Follower's state becomes stale (missing blocks from leader)
- When connection restored, next sync brings follower up to date

**Leader loses connection to follower:**
- Leader continues with partial cluster view
- Leader logs warning about unreachable follower
- Merged state excludes unreachable follower's blocks
- When connection restored, follower's blocks are included again

### State Cleanup

**Expired entries:**
- Persistence compaction removes expired blocks periodically
- Sync naturally propagates removals (expired IPs not in state)
- No explicit "delete" messages needed

**Memory management:**
- Each node maintains full cluster state (~25 MB for 250K IPs)
- Acceptable memory overhead for modern servers
- Compaction keeps memory usage bounded

---

## Monitoring and Metrics

### Metrics to Track

**Leader:**
- `cluster_state_sync_duration_seconds` - Time to collect and merge states
- `cluster_state_sync_nodes_queried` - Number of followers queried
- `cluster_state_sync_nodes_failed` - Number of followers unreachable
- `cluster_state_merged_ips_total` - Total IPs in merged state
- `cluster_state_sync_last_success_timestamp` - Last successful sync

**Follower:**
- `cluster_state_sync_duration_seconds` - Time to fetch and merge from leader
- `cluster_state_sync_ips_added` - New IPs added from leader
- `cluster_state_sync_ips_updated` - Existing IPs updated from leader
- `cluster_state_sync_last_success_timestamp` - Last successful sync

### Logging

**Leader:**
```
[STATE_MERGE] Collecting state from 2 followers
[STATE_MERGE] Merged 245,123 IPs from cluster (leader: 120K, follower-1: 125K)
[STATE_MERGE] Failed to fetch state from follower-2: connection timeout
```

**Follower:**
```
[STATE_SYNC] Synced from leader: 5,234 new, 1,023 updated (total: 245,123)
[STATE_SYNC] Failed to fetch merged state from leader: connection refused
```

---

## Testing Strategy

### Unit Tests

1. **Reason merging:**
   - Same IP blocked by multiple nodes
   - Verify no duplication in merged reasons
   - Verify source node attribution

2. **Conflict resolution:**
   - Different expiry times → keep longest
   - Different reasons → merge both
   - Same reason from multiple nodes → deduplicate

3. **State merging:**
   - Empty states
   - Overlapping states
   - Disjoint states

### Integration Tests

1. **2-node cluster:**
   - Leader blocks IP, verify follower receives it
   - Follower blocks IP, verify leader receives it
   - Both block same IP, verify merged reason

2. **Network failures:**
   - Follower unreachable → leader continues with partial state
   - Leader unreachable → follower uses stale state
   - Connection restored → state converges

3. **Large scale:**
   - 250K IPs in cluster
   - Measure sync duration and bandwidth
   - Verify compression effectiveness

---

## Future Enhancements

### Incremental Sync (Not Yet Implemented)

**Goal:** Only transfer IPs that changed since last sync

**Requirements:**
1. Add `ModifiedAt time.Time` to `persistence.IPState`
2. Update `ModifiedAt` on every block/unblock
3. Filter by `since` parameter in endpoints
4. Track last sync timestamp per node

**Benefits:**
- Reduced bandwidth (only deltas)
- Faster sync cycles
- Better scalability for large clusters

**Implementation notes:**
- TODOs marked in `internal/server/handlers_statesync.go`
- Config option `incremental: true` already exists
- Backward compatible (falls back to full sync)

### Other Potential Features

**State Versioning:**
- Detect conflicts with version numbers
- Resolve based on vector clocks

**Bloom Filters:**
- Optimize which node to query for specific IP
- Reduce unnecessary HTTP requests

**Event-Driven Push:**
- Real-time state updates instead of polling
- WebSocket or SSE for instant propagation

**Multi-Region Support:**
- Handle high-latency clusters
- Regional state aggregation

---

## Migration Path

### Phase 1: Full State Sync (✅ Complete)
- Implement basic full state sync with compression
- Deploy to production
- Monitor bandwidth and performance

### Phase 2: Optimization (Future)
- Add modification time tracking
- Implement incremental sync
- Gradual rollout with feature flag

### Phase 3: Advanced Features (Optional)
- State versioning for conflict detection
- Bloom filters for efficient lookups
- Multi-region support

---

## Comparison with Alternatives

| Approach | Bandwidth | Complexity | Dependencies | Data Loss Risk |
|----------|-----------|------------|--------------|----------------|
| **Shared Filesystem** | None | Low | NFS/GlusterFS | None |
| **Redis/etcd** | Low | Medium | Redis/etcd | None |
| **Full State Sync** | Medium | Low | None | None |
| **Incremental Sync** | Low | Medium | None | None |
| **Query-on-Demand** | Very Low | Low | None | High (node down) |

**Why Persistence-Based Sync?**
- ✅ No external dependencies (no shared storage, no Redis)
- ✅ Leverages existing persistence infrastructure
- ✅ Simple to implement and maintain
- ✅ No data loss (all nodes have complete state)
- ✅ Acceptable bandwidth with compression
- ✅ Self-healing (always converges to correct state)

---

## References

- [Cluster Configuration](ClusterConfiguration.md) - Cluster setup and node roles
- [Persistence](Persistence.md) - State persistence format and journal
- [API Documentation](API.md) - Cluster API endpoints
- [Website Support](WebsiteSupport.md) - Website context in block reasons
