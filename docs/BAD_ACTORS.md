# Bad Actors Feature Plan

**Prerequisite:** SQLite migration must be completed first (see [SQLITE.md](SQLITE.md)).

This document was extracted from the original combined SQLITE.md plan to track the bad actors feature separately.

## Overview

Track IPs that are blocked multiple times across chains/websites. When an IP's score reaches a threshold, promote it to "bad actor" status for permanent blocking.

## Schema (additions to existing SQLite database)

**ip_scores** - Bad actor score tracking (temporary, cleaned up after retention)
```sql
CREATE TABLE ip_scores (
    ip TEXT PRIMARY KEY,
    score REAL NOT NULL DEFAULT 0.0,
    block_count INTEGER NOT NULL DEFAULT 0,
    last_block_time TIMESTAMP NOT NULL
);
CREATE INDEX idx_ip_scores_score ON ip_scores(score);
CREATE INDEX idx_ip_scores_last_block_time ON ip_scores(last_block_time);
```

**bad_actors** - Permanent bad actors (stores IP directly with history)
```sql
CREATE TABLE bad_actors (
    ip TEXT PRIMARY KEY,
    promoted_at TIMESTAMP NOT NULL,
    total_score REAL NOT NULL,
    block_count INTEGER NOT NULL,
    history_json TEXT  -- JSON array of block events leading to promotion
);
CREATE INDEX idx_bad_actors_promoted_at ON bad_actors(promoted_at);
```

These tables are added via a schema migration (version 2) on the existing `state.db`.

## Chain Weights

Different chains contribute different amounts to the bad actor score:
- Critical chains (SQL injection, RCE): weight = 1.0
- Medium severity (rate limiting): weight = 0.5
- Low severity (suspicious UA): weight = 0.3

## Configuration

```yaml
bad_actors:
  enabled: true
  threshold: 5.0  # score needed to become bad actor
  block_duration: "168h"  # 1 week (max available HAProxy table)
  max_score_entries: 100000

chains:
  - name: "SQL-Injection"
    action: block
    block_duration: "1h"
    bad_actor_weight: 1.0  # default if not specified
    steps: [...]
  
  - name: "Suspicious-User-Agent"
    action: block
    block_duration: "30m"
    bad_actor_weight: 0.3  # lower weight
    steps: [...]
```

### Config Types

Add `BadActors` struct to `internal/config/types.go`:
- Fields: Enabled, Threshold (float64), BlockDuration, MaxScoreEntries
- Validation: threshold > 0, durations valid, weight 0.0-1.0
- Add to config hot-reload

Add `BadActorWeight float64` field to `BehavioralChain` struct:
- Default value: 1.0 if not specified
- Validate: 0.0 <= weight <= 1.0

## Scoring Examples

**Example 1: Critical attacks**
- IP blocked 5 times by "SQL-Injection" (weight 1.0)
- Score: 5 × 1.0 = 5.0 → **Bad actor!**

**Example 2: Mixed severity**
- 2× "SQL-Injection" (weight 1.0) = 2.0
- 10× "Suspicious-UA" (weight 0.3) = 3.0
- Total score: 5.0 → **Bad actor!**

**Example 3: Low severity only**
- 10× "Suspicious-UA" (weight 0.3)
- Score: 10 × 0.3 = 3.0 → Not bad actor yet

## Workflow

1. **Block event occurs**
   - Insert into `events` table with chain weight
   - Update `ip_scores` table: increment score by weight
   - Check if score >= threshold

2. **Threshold reached**
   - Insert into `bad_actors` table
   - Issue max-duration block command to HAProxy
   - Log promotion event

3. **Log processing**
   - Check `bad_actors` table (fast indexed lookup)
   - If bad actor: skip chains, ensure block is active
   - Add "bad-actor" skip reason to metrics

4. **Removal**
   - API endpoint: `DELETE /ip/{ip}/clear`
   - Delete from `bad_actors` table
   - Reset score in `ip_scores` table (or delete entry)
   - Remove from HAProxy tables
   - Cluster-aware: broadcasts to all nodes

5. **Cleanup (runs at `compaction_interval`, default hourly)**
   - Delete low scores from `ip_scores` (< 2.0, > 30 days old)
   - Bad actors never cleaned up (permanent until manual removal)

## Processing Priority Order

When processing log entries, check in this order:

1. **Good actors** (from config) - Skip all processing, optionally unblock
2. **Bad actors** (from database) - Skip chain processing, ensure blocked
3. **Normal processing** - Run behavioral chains

```go
// In CheckChains()
if IsGoodActor(ip) {
    // Skip everything, optionally unblock
    return
}

if IsBadActor(ip) {
    // Skip chains, ensure block is active
    EnsureBadActorBlocked(ip)
    return
}

// Normal chain processing
for _, chain := range chains {
    ProcessChain(entry, chain)
}
```

### Good Actor vs Bad Actor Conflict

**Scenario:** IP is in both good_actors config and bad_actors database

**Resolution:** Good actors take priority
- Remove from bad_actors database
- Issue unblock command
- Log: "IP X removed from bad actors (added to good actors)"

This can happen during config reload when an IP is added to good_actors.

## State Restoration on Startup

In addition to the existing SQLite restoration (blocked IPs, unblocked IPs):

**Load bad actors from database:**
```sql
SELECT ip, promoted_at, total_score FROM bad_actors;
```

**Restore to HAProxy:**
```go
// Use max duration from config (e.g., 168h = 1 week)
maxDuration := p.Config.BadActors.BlockDuration
blocker.Block(ipInfo, maxDuration, "bad-actor")
```

## API Endpoints

### New Endpoints

**GET /api/v1/bad-actors**
- List all bad actors (JSON)
- Response includes: ip, promoted_at, total_score, block_count, history

**GET /api/v1/bad-actors/export**
- Export bad actors as plain text (one IP per line)
- For integration with external systems

### Enhanced Endpoints

**GET /ip/{ip}**
- Show current state, score progress, and bad actor status
- `bad_actor` section present only if IP is in `bad_actors` table (its presence indicates bad actor status)
- `score` section present only if IP has entries in `ip_scores` table
- Example response (bad actor):
```json
{
  "ip": "1.2.3.4",
  "current_state": {
    "state": "blocked",
    "expire_time": "2026-03-19T18:00:00Z",
    "reason": "bad-actor"
  },
  "bad_actor": {
    "promoted_at": "2026-03-12T16:00:00Z",
    "total_score": 5.5,
    "block_count": 7,
    "history": [
      {"timestamp": "2026-03-12T16:00:00Z", "reason": "Login-Brute[api]", "weight": 0.5},
      {"timestamp": "2026-03-11T14:30:00Z", "reason": "Path-Traversal", "weight": 1.0}
    ]
  }
}
```
- Example response (not yet bad actor, has score):
```json
{
  "ip": "5.6.7.8",
  "current_state": {
    "state": "blocked",
    "expire_time": "2026-03-19T19:00:00Z",
    "reason": "SQL-Injection"
  },
  "score": {
    "current_score": 3.0,
    "block_count": 3,
    "threshold": 5.0
  }
}
```

**DELETE /ip/{ip}/clear**
- Delete from `bad_actors` table
- Reset score in `ip_scores` table
- Clear from HAProxy and persistence
- Cluster-aware: broadcast to followers

## Metrics

```go
BadActorPromotions    atomic.Int64 `metric:"Bad Actor Promotions" dryrun:"false"`
BadActorChecks        atomic.Int64 `metric:"Bad Actor Checks" dryrun:"false"`
BadActorHits          atomic.Int64 `metric:"Bad Actor Hits" dryrun:"false"`
```

Add to `/stats` endpoint:
```
Bad Actors:
  Promotions:     56
  Checks:         1,234,567
  Hits:           890
  Active:         56
```

## Storage Estimates

**Bad actors storage (1000 bad actors with history):**
- IP (TEXT): ~15 bytes avg per IP
- Metadata: 32 bytes per IP
- History JSON (avg 50 events): ~3 KB per IP
- **Total: ~3 MB for 1000 bad actors**

**Score tracking (100k IPs):**
- IP (TEXT): ~15 bytes avg
- Score + count + timestamp: ~24 bytes
- **Total: ~4 MB for 100k scored IPs**

## Cluster Considerations

- Bad actor scoring is global across all websites (not per-website)
- An IP that's malicious on one site is likely malicious on all
- Events are synced across nodes, scores calculated from all events
- Promotion happens independently on each node when threshold is reached
- Bad actors are synced across cluster (full sync, small table)
- Each node restores bad actors independently from its local database

### Sync Strategy
- **Bad actors table**: Full sync (small table, ~KB)
- **ip_scores**: Not synced directly — derived from events
- **Events with weights**: Synced via existing event sync mechanism

## Config Hot-Reload Behavior

### Bad Actor Threshold Changes
- **Behavior:** Apply to new blocks only
- **Existing scores:** Not recalculated
- **Rationale:** Avoid retroactive promotions/demotions

### Chain Weight Changes
- **Behavior:** Apply immediately to new blocks
- **Existing events:** Keep original weight
- **Rationale:** Historical data should not change

### Bad Actor Block Duration Changes
- **Behavior:** Apply to new bad actor promotions
- **Existing bad actors:** Keep original duration
- **Rationale:** Consistency with original decision

## Implementation Tasks

### Task BA-1: Add bad_actor_weight to chain configuration
- Add `BadActorWeight float64` field to `BehavioralChain` struct in `config/types.go`
- Default value: 1.0 if not specified
- Validate: 0.0 <= weight <= 1.0
- Parse from YAML configuration
- Add to config validation

### Task BA-2: Add bad_actors configuration section
- Add `BadActors` struct to `internal/config/types.go`
- Fields: Enabled, Threshold (float64), BlockDuration, MaxScoreEntries
- Add validation (threshold > 0, durations valid)
- Load config on startup
- Add to config hot-reload

### Task BA-3: Add ip_scores and bad_actors tables (schema migration v2)
- Add migration v2 to create `ip_scores` and `bad_actors` tables
- Add SQLite functions:
  - `IncrementScore(db, ip, weight, timestamp)`
  - `GetScore(db, ip)`
  - `CleanupLowScores(db, retentionPeriod, maxEntries)`
  - `PromoteToBadActor(db, ip, score, blockCount, timestamp)` — queries recent events for history JSON
  - `IsBadActor(db, ip)` — fast indexed lookup
  - `GetAllBadActors(db)`
  - `RemoveBadActor(db, ip)`

### Task BA-4: Implement scoring and promotion logic
- Call `IncrementScore()` in `executeBlock()` with chain's `BadActorWeight`
- Check threshold after increment
- Promote to bad actor when threshold reached
- Issue max-duration block on promotion

### Task BA-5: Integrate bad actor check into chain processing
- Check `IsBadActor()` before chain processing in `CheckChains()`
- Skip chains if bad actor, ensure block is active
- Add skip reason "bad-actor" to metrics
- Handle good actor vs bad actor conflict (good actor wins)
- Add bad actor restoration on startup

### Task BA-6: Add API endpoints
- `GET /api/v1/bad-actors` — list all
- `GET /api/v1/bad-actors/export` — plain text export
- Update `GET /ip/{ip}` — add score and bad_actor sections
- Update `DELETE /ip/{ip}/clear` — remove from bad_actors, reset score

### Task BA-7: Add cleanup and documentation
- Add `ip_scores` cleanup to existing cleanup routine
- Bad actors never cleaned up (permanent)
- Update documentation

## Future Enhancements

- Analytics dashboard — visualize bad actor trends
- Machine learning — predict bad actors before threshold
- Reputation API — share bad actor data across instances
- Geolocation — track bad actors by country/ASN
- Export formats — CSV, JSON, firewall rules
- Automatic weight tuning — adjust weights based on false positive rate
