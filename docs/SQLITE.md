# SQLite Migration and Bad Actors Implementation Plan

## Overview

This document outlines the plan to migrate from the current journal/snapshot persistence system to SQLite, and add bad actor tracking with weighted scoring.

## Motivation

### Current System Issues
- Manual snapshot/journal management (~1000 lines of code)
- Complex compaction logic (snapshot + journal replay)
- Race conditions requiring careful mutex management
- Linear scans for queries (no indexing)
- Duplicate data (snapshot + journal overlap)

### SQLite Benefits
- **Simplification**: ~900 lines of code removed, ~200 net reduction
- **Performance**: Indexed queries, optimized storage
- **Reliability**: ACID transactions, WAL mode crash recovery
- **Concurrency**: Built-in locking, no manual mutex management
- **Queries**: Rich SQL queries for analytics
- **Storage**: Normalized schema, ~73% reduction in string storage

## Architecture

### Single Database File
**Location:** `{state_dir}/state.db`

**Mode:** WAL (Write-Ahead Logging)
- Crash-safe transactions
- Better read/write concurrency
- Automatic checkpointing

### Schema Design

#### Normalized Tables

**reasons** - Deduplicated reason strings with hash-based IDs
```sql
CREATE TABLE reasons (
    id INTEGER PRIMARY KEY,  -- FNV-1a 64-bit hash of reason text (cluster-safe)
    reason TEXT UNIQUE NOT NULL
);
CREATE INDEX idx_reason_text ON reasons(reason);
```

**ips** - Current state per IP (replaces IPStates map)
```sql
CREATE TABLE ips (
    ip TEXT PRIMARY KEY,  -- IP as text (no FK, cluster-safe)
    state TEXT CHECK(state IN ('blocked', 'unblocked')),
    expire_time TIMESTAMP,
    reason_id INTEGER REFERENCES reasons(id),
    modified_at TIMESTAMP,
    first_blocked_at TIMESTAMP
);
CREATE INDEX idx_ips_state ON ips(state);
CREATE INDEX idx_ips_expire_time ON ips(expire_time);
```

**ip_scores** - Bad actor score tracking (temporary, cleaned up after retention)
```sql
CREATE TABLE ip_scores (
    ip TEXT PRIMARY KEY,  -- IP as text (no FK, cluster-safe)
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
    ip TEXT PRIMARY KEY,  -- IP as text (consistent with other tables, cluster-safe)
    promoted_at TIMESTAMP NOT NULL,
    total_score REAL NOT NULL,
    block_count INTEGER NOT NULL,
    history_json TEXT  -- JSON array of block events leading to promotion
);
CREATE INDEX idx_bad_actors_promoted_at ON bad_actors(promoted_at);
```

**events** - Event history (replaces events.log, temporary, cleaned up after retention)
```sql
CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TIMESTAMP NOT NULL,
    event_type TEXT NOT NULL CHECK(event_type IN ('block', 'unblock')),
    ip TEXT NOT NULL,  -- IP as text (no FK, cluster-safe)
    reason_id INTEGER REFERENCES reasons(id),
    weight REAL DEFAULT 1.0,
    node_name TEXT,  -- Node that created this event (for cluster visibility)
    UNIQUE(timestamp, ip, node_name, event_type)  -- Prevent duplicate events during sync
);
CREATE INDEX idx_events_ip_timestamp ON events(ip, timestamp);
CREATE INDEX idx_events_timestamp ON events(timestamp);
CREATE INDEX idx_events_node_name ON events(node_name);
```

**schema_version** - Schema versioning for migrations
```sql
CREATE TABLE schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);
INSERT INTO schema_version (version, description) VALUES (1, 'Initial schema');
```

### Schema Migration System

**Migration hooks** allow upgrading the database schema without data loss.

**Migration structure:**
```go
type Migration struct {
    Version     int
    Description string
    Up          func(*sql.Tx) error  // Upgrade to this version
    Down        func(*sql.Tx) error  // Rollback to previous version (optional)
}
```

**Example migrations:**
```go
var migrations = []Migration{
    {
        Version:     1,
        Description: "Initial schema",
        Up: func(tx *sql.Tx) error {
            // Create all tables (reasons, ips, ip_scores, bad_actors, events, schema_version)
            return createInitialSchema(tx)
        },
    },
    // Future migration example:
    // {
    //     Version:     2,
    //     Description: "Add source column to events",
    //     Up: func(tx *sql.Tx) error {
    //         _, err := tx.Exec("ALTER TABLE events ADD COLUMN source TEXT")
    //         return err
    //     },
    // },
}
```

**Migration execution:**
```go
func ApplyMigrations(db *sql.DB) error {
    // Get current version
    currentVersion := getCurrentVersion(db)
    
    // Apply pending migrations
    for _, migration := range migrations {
        if migration.Version > currentVersion {
            log.Printf("Applying migration %d: %s", migration.Version, migration.Description)
            
            // Start transaction
            tx, _ := db.Begin()
            
            // Apply migration
            if err := migration.Up(tx); err != nil {
                tx.Rollback()
                return fmt.Errorf("migration %d failed: %w", migration.Version, err)
            }
            
            // Update version
            _, err := tx.Exec("INSERT INTO schema_version (version, description) VALUES (?, ?)",
                migration.Version, migration.Description)
            if err != nil {
                tx.Rollback()
                return err
            }
            
            tx.Commit()
            log.Printf("Migration %d applied successfully", migration.Version)
        }
    }
    
    return nil
}
```

**On startup:**
1. Open database
2. Check if `schema_version` table exists
3. If not, create initial schema (version 1)
4. If exists, get current version
5. Apply pending migrations in order
6. Continue with normal operation

### Storage Efficiency

**Reason deduplication with hash-based IDs (50 unique reasons):**
- Without deduplication: 400k events × 30 bytes = 12 MB
- With hash IDs: 400k events × 8 bytes + 50 reasons × 30 bytes = 3.2 MB
- **Savings: 73%**

**IP storage (100k IPs, 400k events):**
- IPs in ips table: 100k × 15 bytes (avg) = 1.5 MB
- IPs in events table: 400k × 15 bytes = 6 MB
- **Total: ~7.5 MB**

**Bad actors storage (1000 bad actors with history):**
- IP (TEXT): ~15 bytes avg per IP
- Metadata: 32 bytes per IP
- History JSON (avg 50 events): ~3 KB per IP
- **Total: ~3 MB for 1000 bad actors**

**Overall database size estimate:**
- IPs: 7.5 MB
- Reasons: 1.5 KB
- Bad actors: 3 MB
- Indexes: ~2 MB
- **Total: ~12.5 MB for 100k IPs, 400k events, 1000 bad actors**

**Key design decisions:**
- IP as TEXT primary key - no AUTOINCREMENT conflicts across cluster nodes
- Reason ID as hash (FNV-1a 64-bit) - deterministic, cluster-safe, no coordination needed
- `bad_actors` table stores IP as TEXT (consistent with other tables) - survives cleanup of other tables
- `bad_actors.history_json` stores complete block history at promotion time
- Aggressive cleanup possible - old events deleted after retention period
- Bad actor history preserved permanently in JSON format

## Bad Actors Feature

### Concept
Track IPs that are blocked multiple times across chains/websites. When an IP's score reaches a threshold, promote it to "bad actor" status for permanent blocking.

### Chain Weights
Different chains contribute different amounts to the bad actor score:
- Critical chains (SQL injection, RCE): weight = 1.0
- Medium severity (rate limiting): weight = 0.5
- Low severity (suspicious UA): weight = 0.3

### Configuration

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

### Scoring Examples

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

### Workflow

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
   - Delete old events (> retention period)
   - Delete expired blocks from `ips`
   - Delete low scores from `ip_scores` (< 2.0, > 30 days old)
   - Delete orphaned `reasons`
   - Bad actors never cleaned up (permanent until manual removal)

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

## State Restoration on Startup

When bot-detector starts, it must restore the blocking state to HAProxy to ensure consistency.

### Restoration Sequence

**1. Load blocked IPs from database**
```sql
SELECT ip, expire_time, reason_id, first_blocked_at
FROM ips
WHERE state = 'blocked' AND expire_time > datetime('now');
```

**2. Load bad actors from database**
```sql
SELECT ip, promoted_at, total_score
FROM bad_actors;
```

**3. Load unblocked IPs (good actors) from database**
```sql
SELECT ip, reason_id
FROM ips
WHERE state = 'unblocked';
```

**4. Restore to HAProxy**

For each blocked IP:
```go
remaining := time.Until(expireTime)
if remaining > 0 {
    blocker.Block(ipInfo, remaining, reason)
}
```

For each bad actor:
```go
// Use max duration from config (e.g., 168h = 1 week)
maxDuration := p.Config.BadActors.BlockDuration
blocker.Block(ipInfo, maxDuration, "bad-actor")
```

For each unblocked IP:
```go
blocker.Unblock(ipInfo, reason)
```

**5. Log restoration stats**
```
[STATE_RESTORE] Restored 1234 blocked IPs, 56 bad actors, 78 unblocked IPs to HAProxy
```

### Priority Order

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

### Cluster Considerations

- Each node restores independently from its local database
- Bad actors are synced across cluster, so all nodes restore same bad actors
- Blocked IPs may differ per node (each node blocks based on its own detection)
- State restoration is idempotent (can run multiple times safely)

## Metrics

### New Metrics for SQLite

Add these fields to `internal/metrics/metrics.go`:

```go
// SQLite and Bad Actor metrics
SQLiteInserts         atomic.Int64 `metric:"SQLite Inserts" dryrun:"false"`
SQLiteUpdates         atomic.Int64 `metric:"SQLite Updates" dryrun:"false"`
SQLiteQueries         atomic.Int64 `metric:"SQLite Queries" dryrun:"false"`
SQLiteErrors          atomic.Int64 `metric:"SQLite Errors" dryrun:"false"`
BadActorPromotions    atomic.Int64 `metric:"Bad Actor Promotions" dryrun:"false"`
BadActorChecks        atomic.Int64 `metric:"Bad Actor Checks" dryrun:"false"`
BadActorHits          atomic.Int64 `metric:"Bad Actor Hits" dryrun:"false"`
DatabaseSizeBytes     atomic.Int64 `metric:"Database Size (bytes)" dryrun:"false"`
CleanupRowsDeleted    atomic.Int64 `metric:"Cleanup Rows Deleted" dryrun:"false"`
```

**Query latency tracking:**
- Track query duration in milliseconds
- Calculate p50, p95, p99 percentiles (simple moving average)
- Store in atomic counters
- Update on each query

**Database size tracking:**
- Query `PRAGMA page_count` and `PRAGMA page_size` every 5 minutes
- Calculate: `size = page_count * page_size`
- Update `DatabaseSizeBytes` gauge

### Metrics Display

Add to `/stats` endpoint:
```
SQLite Operations:
  Inserts:        12,345
  Updates:        5,678
  Queries:        98,765
  Errors:         3
  Database Size:  12.5 MB

Bad Actors:
  Promotions:     56
  Checks:         1,234,567
  Hits:           890
  Active:         56
```

## Error Handling Strategy

### Database Initialization Errors

**Scenario:** Database file cannot be opened (permissions, corruption, disk full)

**Strategy:**
1. Log error at ERROR level
2. Disable persistence (`PersistenceEnabled = false`)
3. Continue with in-memory only mode
4. Set unhealthy flag for health check endpoint

**Example:**
```go
db, err := InitSQLiteDB(stateDir, dryRun)
if err != nil {
    p.LogFunc(logging.LevelError, "SQLITE_INIT_FAIL", "Failed to initialize SQLite: %v. Running in-memory only.", err)
    p.PersistenceEnabled = false
    p.SQLiteHealthy = false
    return nil // Continue without persistence
}
```

### Query Errors

**Scenario:** SELECT query fails (corruption, lock timeout)

**Strategy:**
1. Log error at WARNING level
2. Return empty result or default value
3. Continue processing (don't block)
4. Metrics: increment `SQLiteErrors`

**Example:**
```go
state, err := GetIPState(ip)
if err != nil {
    p.LogFunc(logging.LevelWarning, "SQLITE_QUERY_FAIL", "Failed to query IP state for %s: %v", ip, err)
    p.Metrics.SQLiteErrors.Add(1)
    return nil, nil // Return empty, continue processing
}
```

### Insert/Update Errors

**Scenario:** INSERT/UPDATE fails (disk full, constraint violation)

**Strategy:**
1. Log error at WARNING level
2. Skip persistence for this event
3. Continue processing (event still processed, just not persisted)
4. Metrics: increment `SQLiteErrors`

**Example:**
```go
err := InsertEvent(timestamp, eventType, ip, reason, weight, nodeName)
if err != nil {
    p.LogFunc(logging.LevelWarning, "SQLITE_INSERT_FAIL", "Failed to insert event for %s: %v", ip, err)
    p.Metrics.SQLiteErrors.Add(1)
    // Continue - event processed, just not persisted
}
```

### Database Corruption

**Scenario:** Database file is corrupted (detected via `PRAGMA integrity_check`)

**Strategy:**
1. Log error at ERROR level
2. Attempt to backup corrupted DB: `mv state.db state.db.corrupted.{timestamp}`
3. Create new empty database
4. Continue with fresh state
5. Alert operator (log at CRITICAL level)

**Example:**
```go
func CheckDatabaseIntegrity(db *sql.DB) error {
    var result string
    err := db.QueryRow("PRAGMA integrity_check").Scan(&result)
    if err != nil || result != "ok" {
        return fmt.Errorf("database integrity check failed: %s", result)
    }
    return nil
}

// On startup
if err := CheckDatabaseIntegrity(db); err != nil {
    p.LogFunc(logging.LevelError, "SQLITE_CORRUPT", "Database corrupted: %v. Creating new database.", err)
    backupPath := fmt.Sprintf("%s.corrupted.%d", dbPath, time.Now().Unix())
    os.Rename(dbPath, backupPath)
    db, _ = InitSQLiteDB(stateDir, dryRun) // Create fresh DB
}
```

### Health Check

**Continuous monitoring:**
```go
func StartHealthCheck(db *sql.DB, p *Processor) {
    ticker := time.NewTicker(30 * time.Second)
    go func() {
        for range ticker.C {
            var result int
            err := db.QueryRow("SELECT 1").Scan(&result)
            if err != nil {
                p.LogFunc(logging.LevelWarning, "SQLITE_HEALTH", "Database health check failed: %v", err)
                p.SQLiteHealthy = false
            } else {
                p.SQLiteHealthy = true
            }
        }
    }()
}
```

### Fallback Behavior

**If database becomes unavailable during runtime:**
1. Continue processing logs (blocking still works via HAProxy)
2. Skip persistence operations (log warnings)
3. Bad actor checks fail-open (don't block if can't query)
4. Health endpoint returns unhealthy
5. Operator alerted via logs/metrics

**Priority:** Availability > Persistence (keep processing even if DB fails)

### Initial Sync
**Endpoint:** `GET /api/v1/cluster/state/db-dump`

Follower fetches full state from leader:
```sql
-- Leader generates SQL dump
SELECT * FROM reasons;
SELECT * FROM ips;
SELECT * FROM ip_scores;
SELECT * FROM bad_actors;
SELECT * FROM events WHERE timestamp > ?;
```

Returns as JSON array of rows, follower executes INSERTs in transaction.

### Incremental Sync
**Endpoint:** `GET /api/v1/cluster/state/db-dump?since={timestamp}`

Only fetch new events since last sync:
```sql
-- Events (with IP text, hash-based reason IDs, and node info)
SELECT e.timestamp, e.event_type, e.ip, r.reason, e.weight, e.node_name
FROM events e
LEFT JOIN reasons r ON r.id = e.reason_id
WHERE e.timestamp > ?;

-- Reasons (sync separately to ensure IDs match)
SELECT id, reason FROM reasons;
```

Follower applies new events:
```sql
-- Insert reasons first (idempotent with hash-based IDs)
INSERT OR IGNORE INTO reasons (id, reason) VALUES (?, ?);

-- Insert events (OR IGNORE handles duplicates via UNIQUE constraint)
INSERT OR IGNORE INTO events (timestamp, event_type, ip, reason_id, weight, node_name) 
VALUES (?, ?, ?, ?, ?, ?);

-- Update ips table based on events
-- (application logic handles state updates)
```

**Deduplication:** The `UNIQUE(timestamp, ip, node_name, event_type)` constraint ensures each node's events are only inserted once, even if sync runs multiple times.

### Compression
Both initial and incremental sync endpoints should support gzip compression (`Accept-Encoding: gzip`) to reduce transfer size, matching the current system's compression support for state sync.

### Sync Strategy
- **Initial sync**: Full state on startup
- **Incremental sync**: Every 30 seconds (configurable)
- **Bad actors**: Full sync (small table, ~KB)
- **Events**: Incremental only (large table, ~MB)

## Migration Strategy

### Automatic Migration on Startup

1. **Check for legacy files**
   - Look for `snapshot.json` and `events.log` in state directory

2. **Import snapshot**
   ```go
   snapshot := LoadSnapshot("snapshot.json")
   for ip, state := range snapshot.IPStates {
       reasonID := getOrCreateReasonID(state.Reason)  // Hash-based ID
       db.Exec("INSERT INTO ips (ip, state, expire_time, reason_id, modified_at, first_blocked_at) VALUES (?, ?, ?, ?, ?, ?)", 
               ip, state.State, state.ExpireTime, reasonID, state.ModifiedAt, state.FirstBlockedAt)
   }
   ```

3. **Import journal**
   ```go
   scanner := bufio.NewScanner(journalFile)
   for scanner.Scan() {
       var entry JournalEntryV1
       json.Unmarshal(line, &entry)
       reasonID := getOrCreateReasonID(entry.Event.Reason)  // Hash-based ID
       weight := inferWeightFromReason(entry.Event.Reason) // default 1.0
       nodeName := "" // Legacy entries don't have node info
       db.Exec("INSERT INTO events (timestamp, event_type, ip, reason_id, weight, node_name) VALUES (?, ?, ?, ?, ?, ?)", 
               entry.Timestamp, entry.Event.Type, entry.Event.IP, reasonID, weight, nodeName)
   }
   ```

4. **Rename legacy files**
   - `snapshot.json` → `snapshot.json.migrated`
   - `events.log` → `events.log.migrated`

5. **Continue with SQLite**
   - All future operations use SQLite only

### Rollback Plan
If migration fails or issues found:
1. Rename `.migrated` files back to original names
2. Disable SQLite in code (feature flag)
3. Restart with legacy system

## Code Simplification

### Files Removed
- `internal/persistence/io.go` (~300 lines) - snapshot read/write
- `internal/persistence/journal.go` (~200 lines) - journal operations
- Compaction logic in `main.go` (~100 lines)
- Complex mutex management (~50 lines)

### Files Modified
- `internal/persistence/types.go` - Keep type definitions, remove I/O
- `cmd/bot-detector/main.go` - Remove compaction, simplify startup
- `internal/checker/checker.go` - Replace map access with DB calls
- `internal/cluster/syncloop.go` - Simplify state sync

### New Files
- `internal/persistence/sqlite.go` (~500 lines) - All SQLite operations
- `internal/badactors/badactors.go` (~200 lines) - Bad actor logic

### Net Change
- **Lines removed:** ~900
- **Lines added:** ~700
- **Net reduction:** ~200 lines (plus much simpler logic)

## Implementation Tasks

### Phase 1: SQLite Foundation (Tasks 1-3)

**Task 1: Add SQLite dependency and initialize database**
- Add `modernc.org/sqlite` to go.mod (pure Go, no CGO)
- Create `internal/persistence/sqlite.go`
- Add `InitSQLiteDB(stateDir, dryRun)` function
- Enable WAL mode: `PRAGMA journal_mode=WAL`
- Set `PRAGMA synchronous=NORMAL`
- Implement schema migration system:
  - Define `Migration` struct with Version, Description, Up, Down functions
  - Create migrations array with version 1 (initial schema)
  - Add `ApplyMigrations(db)` function to run pending migrations in transaction
  - Check `schema_version` table on startup, apply migrations if needed
- Execute schema creation SQL (via migration v1)
- Add helper function: `getOrCreateReasonID(reason)` - uses FNV-1a hash
  - Calculate hash: `h := fnv.New64a(); h.Write([]byte(reason)); id := int64(h.Sum64())`
  - Insert: `INSERT OR IGNORE INTO reasons (id, reason) VALUES (?, ?)`
  - Verify no collision (check reason text matches)
- Add SQLite metrics to Processor.Metrics:
  - `SQLiteInserts` - counter for INSERT operations
  - `SQLiteUpdates` - counter for UPDATE operations
  - `SQLiteQueries` - counter for SELECT operations
  - `SQLiteErrors` - counter for database errors
  - `BadActorPromotions` - counter for IPs promoted to bad actors
  - `BadActorChecks` - counter for bad actor lookups
  - `BadActorHits` - counter for bad actor matches (skipped chains)
  - `DatabaseSizeBytes` - gauge for database file size
  - `CleanupRowsDeleted` - counter for rows deleted during cleanup
- Add error handling:
  - If DB open fails: log error, disable persistence, continue with in-memory only
  - If query fails: log error, return empty result, continue processing
  - If insert fails: log error, skip persistence for this event, continue processing
  - Add health check: `SELECT 1` query every 30s, set unhealthy flag if fails
- Tests:
  - Database initialization and WAL mode verification
  - Schema creation (all tables exist with correct columns)
  - Migration system (apply v1, verify version, re-run is idempotent)
  - `getOrCreateReasonID` deduplication (same reason → same ID)
  - Error handling (init failure disables persistence gracefully)

**Acceptance:** Database created with cluster-safe schema (IP text, hash-based reason IDs), WAL mode enabled, migration system working, metrics tracked, error handling in place, tests pass

**Task 2: Implement events table operations**
- Add `InsertEvent(timestamp, eventType, ip, reason, weight, nodeName)` - uses hash-based reason ID and node name
- Add `GetEventsSince(timestamp)` for cluster sync
- Add `GetEventsForIP(ip, limit)` for API queries - includes node_name
- Add `GetEventsByNode(nodeName, limit)` for node-specific queries
- Add `CleanupOldEvents(retentionPeriod)` for maintenance
- Tests:
  - Insert and retrieve events
  - UNIQUE constraint prevents duplicates (same timestamp, ip, node_name, event_type)
  - `GetEventsSince` returns only events after given timestamp
  - `GetEventsForIP` returns correct events with limit
  - `CleanupOldEvents` deletes only events older than retention period

**Acceptance:** Events stored with IP text, hash-based reason IDs, and node name, queryable efficiently, tests pass

**Task 3: Implement ips table operations**
- Add `UpsertIPState(ip, state, expireTime, reason, modifiedAt, firstBlockedAt)` - uses IP text and hash-based reason ID
- Add `GetIPState(ip)` - returns full state with reason text (JOIN with reasons table)
- Add `DeleteIPState(ip)` - removes from ips table
- Add `GetAllBlockedIPs()` for HAProxy resync
- Add `GetAllIPStates()` for cluster sync
- Tests:
  - Insert, update (upsert), and retrieve IP state
  - `GetIPState` JOIN returns reason text correctly
  - `GetAllBlockedIPs` returns only blocked IPs
  - `DeleteIPState` removes entry
  - Concurrent read/write access (race detector)

**Acceptance:** IP states stored and retrieved efficiently, JOINs work correctly, tests pass

### Phase 2: Core Migration (Tasks 4-6)

**Task 4: Replace IPStates map with SQLite**
- Update `executeBlock()` in checker.go to call `InsertEvent()` and `UpsertIPState()`
- Update `executeUnblock()` to call `InsertEvent()` and `UpsertIPState()`
- Replace all `p.IPStates[ip]` reads with `GetIPState(ip)`
- Remove `PersistenceMutex` locking (SQLite handles it)
- Update resync callback to query `GetAllBlockedIPs()`
- Implement state restoration on startup:
  - Query all blocked IPs from `ips` table where `state = 'blocked'` and `expire_time > now()`
  - For each blocked IP: issue block command to HAProxy with remaining duration
  - Query all bad actors from `bad_actors` table
  - For each bad actor: issue block command to HAProxy with max duration (from config)
  - Query all unblocked IPs from `ips` table where `state = 'unblocked'`
  - For each unblocked IP: issue unblock command to HAProxy (good actors)
  - Log restoration stats: "Restored X blocked IPs, Y bad actors, Z unblocked IPs"
- Add good actors priority check:
  - Before checking bad actors, check if IP is in good_actors config
  - If good actor: skip bad actor check, skip chain processing
  - Priority: good_actors > bad_actors > normal processing
- Tests:
  - Block/unblock round-trip through SQLite (executeBlock → GetIPState)
  - State restoration loads blocked IPs and issues correct HAProxy commands
  - Good actor priority (good actor skips all processing)
  - Expired blocks not restored

**Acceptance:** Blocking/unblocking works with SQLite, no map access, state restored to HAProxy on startup, good actors have priority, tests pass

**Task 5: Implement migration from legacy format**
- Add `MigrateFromLegacy(stateDir)` function
- Read `snapshot.json` → insert into `ips` table
- Read `events.log` → insert into `events` table
- Infer weights from reasons (default 1.0)
- Rename files to `.migrated` after success
- Run migration on startup if legacy files exist
- Add error handling and rollback
- Tests:
  - Migration from snapshot with known data, verify all IPs in `ips` table
  - Migration from journal with known events, verify all events in `events` table
  - Legacy files renamed to `.migrated` after success
  - Migration skipped if no legacy files exist
  - Migration skipped if database already has data
  - Rollback on failure (legacy files unchanged)

**Acceptance:** Existing journal/snapshot imported into SQLite successfully, tests pass

**Task 6: Remove legacy persistence code**
- Delete `internal/persistence/io.go`
- Delete `internal/persistence/journal.go`
- Remove compaction logic from `main.go`
- Remove `PersistenceMutex` from Processor struct
- Remove `IPStates map[string]persistence.IPState`
- Update existing tests to use SQLite

**Acceptance:** Legacy code removed, all existing tests updated and passing with SQLite

### Phase 3: Bad Actors (Tasks 7-10)

**Task 7: Add bad_actor_weight to chain configuration**
- Add `BadActorWeight float64` field to `BehavioralChain` struct in `config/types.go`
- Default value: 1.0 if not specified
- Validate: 0.0 <= weight <= 1.0
- Parse from YAML configuration
- Add to config validation
- Tests:
  - Config loads with explicit weight
  - Config defaults to 1.0 when weight omitted
  - Validation rejects weight < 0.0 and > 1.0

**Acceptance:** Chains load with bad_actor_weight, defaults to 1.0, validation works, tests pass

**Task 8: Implement bad actor scoring**
- Add `IncrementScore(ip, weight, timestamp)` - updates score and block_count in ip_scores table
- Add `GetScore(ip)` function
- Add `CleanupLowScores(retentionPeriod, maxEntries)` - remove old low-score IPs
- Call `IncrementScore()` in `executeBlock()` with chain's `BadActorWeight`
- Check threshold after increment
- Tests:
  - Score increments correctly (multiple weights accumulate)
  - `block_count` increments on each call
  - `GetScore` returns correct score for known IP
  - `GetScore` returns zero/nil for unknown IP
  - Threshold detection (score crosses threshold after increment)
  - `CleanupLowScores` removes old low-score entries, keeps high scores

**Acceptance:** Scores tracked and incremented with weights, threshold detection works, tests pass

**Task 9: Implement bad actor promotion and matching**
- Add `PromoteToBadActor(ip, score, blockCount, timestamp)` function
  - Query recent block history from `events` table (last 50 blocks)
  - Build history JSON: `[{"ts": "...", "r": "reason", "w": 1.0, "n": "node1"}, ...]`
  - Insert into `bad_actors` table with IP, score, count, and history JSON
- Add `IsBadActor(ip)` - fast indexed lookup on bad_actors table
- Add `GetAllBadActors()` for API - returns IP and metadata
- Add `RemoveBadActor(ip)` - deletes from bad_actors table
- Issue max-duration block on promotion
- Check `IsBadActor()` before chain processing in `CheckChains()`
- Skip chains if bad actor, ensure block is active
- Add skip reason "bad-actor" to metrics
- Tests:
  - Promotion inserts into `bad_actors` with correct history JSON
  - `IsBadActor` returns true for promoted IP, false for unknown
  - `GetAllBadActors` returns all promoted IPs
  - `RemoveBadActor` deletes entry, `IsBadActor` returns false after
  - Bad actor skips chain processing (integration with CheckChains)
  - Good actor vs bad actor conflict (good actor wins, bad actor removed)

**Acceptance:** Bad actors promoted at threshold with history JSON, skip chain processing, remain blocked, tests pass

**Task 10: Add bad_actors configuration**
- Add `BadActors` struct to `internal/config/types.go`
- Fields: Enabled, Threshold (float64), BlockDuration, MaxScoreEntries
- Add validation (threshold > 0, durations valid)
- Load config on startup
- Add to config hot-reload
- Add to configuration documentation
- Tests:
  - Config loads with all fields
  - Validation rejects threshold <= 0
  - Validation rejects invalid durations
  - Hot-reload picks up changes

**Acceptance:** Config loads and validates bad_actors section, hot-reload works, tests pass

### Phase 4: API and Cluster (Tasks 11-13)

**Task 11: Add API endpoints for bad actors**
- Add `GET /api/v1/bad-actors` - List all (JSON with ip, promoted_at, total_score, block_count)
- Add `GET /api/v1/bad-actors/export` - Plain text, one IP per line
- Update `GET /ip/{ip}` - Add `score` section (from ip_scores) and `bad_actor` section (from bad_actors, present only if promoted)
- Update `DELETE /ip/{ip}/clear` - Remove from bad_actors, reset score, cluster broadcast
- Add API documentation
- Tests:
  - `GET /api/v1/bad-actors` returns correct list
  - `GET /api/v1/bad-actors/export` returns one IP per line
  - `GET /ip/{ip}` includes `bad_actor` section only when promoted
  - `GET /ip/{ip}` includes `score` section only when IP has scores
  - `DELETE /ip/{ip}/clear` removes bad actor and resets score

**Acceptance:** API endpoints work, show scores and bad actor status, tests pass

**Task 12: Implement cluster database sync**
- Add `GET /api/v1/cluster/state/db-dump` endpoint
- Optional `?since={timestamp}` for incremental sync
- Returns JSON array of rows (reasons, ips, events, bad_actors)
- Follower executes INSERTs in transaction
- Add incremental sync timer (every 30s)
- Handle conflicts (INSERT OR REPLACE)
- Sync bad_actors separately (full sync, small size)
- Tests:
  - Full sync transfers all tables correctly
  - Incremental sync returns only events since timestamp
  - Duplicate sync is idempotent (no errors, no duplicate rows)
  - Bad actors synced correctly
  - Sync with network failure (follower retries)

**Acceptance:** Followers sync state from leader via SQL dumps, incremental sync works, tests pass

**Task 13: Implement cleanup routines**
- Add `RunCleanup()` function called during compaction interval
- Cleanup old events (retention period)
- Cleanup old low-score IPs (retention + max entries)
- Don't cleanup bad actors (separate `bad_actors` table, permanent)
- Run VACUUM periodically (weekly)
- Log cleanup stats (rows deleted, space freed)
- Tests:
  - Old events deleted, recent events kept
  - Low scores cleaned up, high scores kept
  - Bad actors never cleaned up
  - VACUUM reduces database size after deletions
  - Cleanup during active processing (concurrent access with race detector)

**Acceptance:** Old data cleaned up, database size controlled, VACUUM runs, tests pass

### Phase 5: Documentation (Task 14)

**Task 14: Update documentation**
- Update README.md with SQLite information
- Update Configuration.md with bad_actors section
- Update API.md with new endpoints
- Add migration guide for existing deployments
- Add troubleshooting section
- Document schema and design decisions (this file)

**Acceptance:** Documentation complete and accurate

## Testing Strategy

Tests are implemented alongside each task (see task descriptions above). All tasks include specific test requirements in their acceptance criteria.

### Approach
- Each task includes unit tests for new functions and integration tests for workflows
- All tests run with `-race` flag to detect concurrent access issues
- Use `:memory:` SQLite databases in tests for speed and isolation

### Performance Tests (run before production rollout)
- Insert performance (1000 events/sec)
- Query performance (lookup by IP)
- Cleanup performance (delete old events)
- Database size growth over time

### Migration Tests (run before production rollout)
- Import from real production snapshot/journal
- Verify data integrity after migration
- Test rollback procedure

## Rollout Plan

### Development
1. Implement on feature branch
2. Run all tests (unit, integration, performance)
3. Test migration with production data copy
4. Code review

### Staging
1. Deploy to staging environment
2. Run migration on staging data
3. Monitor for 1 week
4. Verify cluster sync works
5. Test API endpoints
6. Performance testing

### Production
1. Backup current state directory
2. Deploy new version
3. Automatic migration on startup
4. Monitor logs for errors
5. Verify blocking still works
6. Check database size and performance
7. Monitor for 24 hours
8. If issues: rollback to legacy system

## Monitoring

### Metrics to Track
- Database size (MB)
- Query latency (ms)
- Insert rate (events/sec)
- Bad actor promotions (count)
- Bad actor hit rate (skips/sec)
- Cleanup stats (rows deleted)

### Alerts
- Database size > 1 GB
- Query latency > 100ms
- Insert errors
- Migration failures
- Cluster sync failures

## Future Enhancements

### Potential Improvements
1. **Analytics dashboard** - Visualize bad actor trends
2. **Machine learning** - Predict bad actors before threshold
3. **Reputation API** - Share bad actor data across instances
4. **Geolocation** - Track bad actors by country/ASN
5. **Export formats** - CSV, JSON, firewall rules
6. **Historical analysis** - Query patterns over time
7. **Automatic weight tuning** - Adjust weights based on false positive rate

## References

- SQLite Documentation: https://www.sqlite.org/docs.html
- SQLite WAL Mode: https://www.sqlite.org/wal.html
- modernc.org/sqlite: https://pkg.go.dev/modernc.org/sqlite
- Go database/sql: https://pkg.go.dev/database/sql


---

# Appendix A: Design Decisions and Rationale

## Components That Remain Unchanged

The following components from the current system remain as-is and do not interact with SQLite:

### ActivityStore (In-Memory Chain Progress)
- **Purpose:** Tracks current step and last match time for active chain processing
- **Scope:** In-memory only, not persisted
- **Relationship to SQLite:** None - separate concern from persistence
- **Cleanup:** Handled by existing `CleanUpIdleActors()` routine

### ReasonCache (String Interning)
- **Purpose:** In-memory string deduplication for memory optimization
- **Scope:** Runtime optimization
- **Relationship to SQLite:** Two-level deduplication (memory + database)
- **Strategy:** Keep both - ReasonCache for hot path, reasons table for persistence

### Good Actors
- **Purpose:** Trusted IPs that skip all processing
- **Storage:** Config file only (not in database)
- **Priority:** Good actors > Bad actors > Normal processing
- **Rationale:** Config-based is simpler and sufficient

### Multi-Website Support
- **Purpose:** Track blocks per website
- **Implementation:** Website context embedded in reason strings (e.g., "Login-Brute[api_site]")
- **Bad Actor Scoring:** Global across all websites (not per-website)
- **Rationale:** An IP that's malicious on one site is likely malicious on all

## Configuration Decisions

### Compaction Interval
**Decision:** Keep `application.persistence.compaction_interval`, repurpose for SQLite cleanup

**Rationale:**
- Backward compatibility
- Same semantic meaning (periodic maintenance)
- Avoids config migration

### Retention Period
**Decision:** Keep in `application.persistence.retention_period`

**Rationale:**
- Already exists in current config
- Applies to both events and ip_scores cleanup
- Consistent with current system

### Dry-Run Mode
**Decision:** Use `:memory:` SQLite database in dry-run mode

**Rationale:**
- Tests full code path (same code as production)
- No disk I/O (fast)
- Automatic cleanup on exit
- Simpler than conditional logic

**How it works:**
```go
func InitSQLiteDB(stateDir string, dryRun bool) (*sql.DB, error) {
    var dbPath string
    if dryRun {
        dbPath = ":memory:"  // In-memory database
    } else {
        dbPath = filepath.Join(stateDir, "state.db")
    }
    
    db, err := sql.Open("sqlite", dbPath)
    // ... rest of initialization
}
```

**Benefits:**
- Can test migration with production data copies safely
- Full SQLite functionality (schema, queries, transactions)
- No cleanup needed (memory freed on exit)
- Fast (no disk I/O bottleneck)

**Example usage:**
```bash
# Test with production data copy
cp /var/lib/bot-detector/state/* /tmp/test/
./bot-detector --dry-run --state-dir /tmp/test --log-path /tmp/test.log
# Database created in RAM, destroyed on exit
```

## Cluster Safety Decisions

### IP Storage
**Decision:** Store IP as TEXT (not INTEGER with AUTOINCREMENT)

**Rationale:**
- No ID conflicts across nodes
- Natural key (IP is already unique)
- Simpler merge logic (INSERT OR REPLACE)
- Storage overhead acceptable (6 MB for 100k IPs)

### Reason IDs
**Decision:** Use FNV-1a 64-bit hash of reason text

**Rationale:**
- Deterministic (same reason → same ID everywhere)
- No coordination needed between nodes
- Collision risk negligible (64-bit space)
- Fast computation (non-cryptographic)

### Event Deduplication
**Decision:** UNIQUE constraint on (timestamp, ip, node_name, event_type)

**Rationale:**
- Prevents duplicate events during sync
- INSERT OR IGNORE is idempotent
- Can sync multiple times safely
- No need for global event IDs

## Error Handling Philosophy

**Priority:** Availability > Persistence

**Rationale:**
- Bot detection must continue even if persistence fails
- Blocking still works via HAProxy (in-memory state)
- Persistence is for recovery, not real-time operation
- Fail-open for bad actor checks (don't block if can't query)

## Performance Considerations

### Why SQLite Over Other Options

**vs PostgreSQL/MySQL:**
- ✅ No external dependencies
- ✅ Simpler deployment
- ✅ Lower resource usage
- ✅ Sufficient for scale (100k IPs, 400k events)

**vs Embedded KV stores (BoltDB, BadgerDB):**
- ✅ SQL queries (more flexible)
- ✅ Better tooling (sqlite3 CLI)
- ✅ Proven reliability
- ✅ Built-in transactions

**vs Current journal/snapshot:**
- ✅ Indexed queries (faster lookups)
- ✅ No compaction needed
- ✅ Better concurrency (WAL mode)
- ✅ Simpler code (~900 lines removed, ~200 net reduction)

### Storage Efficiency

**100k IPs, 400k events, 1000 bad actors:**
- IPs: 7.5 MB
- Reasons: 1.5 KB
- Bad actors: 3 MB
- Indexes: 2 MB
- **Total: ~12.5 MB**

**vs Current system:**
- Snapshot: ~25 MB (gzipped)
- Journal: ~10 MB
- **Total: ~35 MB**

**Savings: 64% smaller**

## Migration Strategy Decisions

### Automatic Migration
**Decision:** Migrate automatically on startup if legacy files detected

**Rationale:**
- Zero-downtime migration
- No manual intervention required
- Rollback possible (keep .migrated files)
- Safe (rename files only after success)

### Schema Versioning
**Decision:** Use migration hooks with version tracking

**Rationale:**
- Future-proof (easy to add new migrations)
- Transactional (atomic upgrades)
- Rollback support (optional Down() function)
- Standard pattern (used by many projects)

### Rollback Window
**Decision:** Keep .migrated files for 7 days

**Rationale:**
- Enough time to detect issues
- Not too long (disk space)
- Can manually revert if needed
- Automatic cleanup after 7 days

---

# Appendix B: Operational Procedures

## Graceful Shutdown Sequence

When bot-detector receives SIGTERM or SIGINT:

1. **Stop accepting new log lines**
   - Close log file handles
   - Stop tailer goroutines

2. **Finish processing in-flight entries**
   - Wait for entry buffer to drain
   - Complete current chain processing

3. **Flush pending database operations**
   - Wait for pending INSERTs/UPDATEs
   - Ensure all transactions committed

4. **Checkpoint WAL**
   ```go
   db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
   ```

5. **Close database connection**
   ```go
   db.Close()
   ```

6. **Log shutdown complete**
   ```
   [SHUTDOWN] Shutdown complete. Database closed cleanly.
   ```

**Timeout:** 30 seconds maximum (force exit after timeout)

## Config Hot-Reload Behavior

When configuration is reloaded:

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

### Database Schema Changes
- **Behavior:** Not supported via hot-reload
- **Requirement:** Restart required for schema migrations
- **Rationale:** Schema changes need careful migration

## Rollback Procedure

If migration fails or issues are detected:

### Step 1: Stop bot-detector
```bash
systemctl stop bot-detector
```

### Step 2: Verify .migrated files exist
```bash
ls -la /var/lib/bot-detector/state/
# Should see: snapshot.json.migrated, events.log.migrated
```

### Step 3: Restore legacy files
```bash
cd /var/lib/bot-detector/state/
mv snapshot.json.migrated snapshot.json
mv events.log.migrated events.log
mv state.db state.db.backup
```

### Step 4: Restart bot-detector
```bash
systemctl start bot-detector
```

### Step 5: Verify operation
```bash
tail -f /var/log/bot-detector/bot-detector.log
# Should see: "Loaded snapshot", "Replayed journal"
```

## Database Maintenance

### Manual Integrity Check
```bash
sqlite3 /var/lib/bot-detector/state/state.db "PRAGMA integrity_check"
```

### Manual Vacuum (Reclaim Space)
```bash
sqlite3 /var/lib/bot-detector/state/state.db "VACUUM"
```

### Backup Database
```bash
sqlite3 /var/lib/bot-detector/state/state.db ".backup /backup/state.db.$(date +%Y%m%d)"
```

### View Database Stats
```bash
sqlite3 /var/lib/bot-detector/state/state.db <<EOF
SELECT 'IPs:', COUNT(*) FROM ips;
SELECT 'Events:', COUNT(*) FROM events;
SELECT 'Bad Actors:', COUNT(*) FROM bad_actors;
SELECT 'Reasons:', COUNT(*) FROM reasons;
SELECT 'DB Size:', page_count * page_size / 1024 / 1024 || ' MB' 
FROM pragma_page_count(), pragma_page_size();
EOF
```

## Monitoring and Alerting

### Health Check Endpoint
```bash
curl http://localhost:8080/health/database
```

**Response (healthy):**
```json
{
  "status": "healthy",
  "database": "ok",
  "last_check": "2026-03-12T18:00:00Z"
}
```

**Response (unhealthy):**
```json
{
  "status": "unhealthy",
  "database": "error",
  "error": "database locked",
  "last_check": "2026-03-12T18:00:00Z"
}
```

### Metrics to Monitor

**Critical:**
- `SQLiteErrors` - Alert if > 0
- `DatabaseSizeBytes` - Alert if > 1 GB
- Database health check - Alert if unhealthy for > 5 minutes

**Warning:**
- Query latency p95 - Alert if > 100ms
- Cleanup rows deleted - Alert if > 100k per run
- Bad actor promotions - Alert if > 100 per hour (potential attack)

### Log Patterns to Watch

**Errors:**
```
[SQLITE_INIT_FAIL] Failed to initialize SQLite
[SQLITE_CORRUPT] Database corrupted
[SQLITE_INSERT_FAIL] Failed to insert event
```

**Warnings:**
```
[SQLITE_QUERY_FAIL] Failed to query IP state
[SQLITE_HEALTH] Database health check failed
```

**Info:**
```
[STATE_RESTORE] Restored X blocked IPs, Y bad actors
[CLEANUP] Deleted X events, Y scores
[BAD_ACTOR_PROMOTE] IP X promoted to bad actor (score: Y)
```

---

# Appendix C: Testing Guidelines

Tests are implemented alongside each task (see Implementation Tasks). This appendix covers cross-cutting testing concerns.

## Race Condition Tests

Run all tests with `-race` flag:
```bash
go test -race ./...
```

**Critical paths:**
- Concurrent INSERTs from multiple goroutines
- Concurrent reads during writes
- Cluster sync during local writes
- Cleanup during active processing

## Performance Tests (pre-production)

- 1000 blocks/second sustained
- 10k concurrent IPs tracked
- 100k events in database
- Query latency under load
- Memory usage compared to current system
- Database size growth over time

## Test Infrastructure

- Use `:memory:` SQLite databases for test isolation and speed
- Each test creates its own database instance (no shared state)
- Helper functions for common setup (create DB, insert test data)

---

# Appendix D: Future Enhancements

## Potential Improvements

### 1. Advanced Analytics
- Query patterns over time
- Identify attack trends
- Correlate bad actors across websites
- Generate reports

### 2. Machine Learning Integration
- Predict bad actors before threshold
- Adjust chain weights automatically
- Anomaly detection

### 3. Reputation API
- Share bad actor data across instances
- Query external reputation databases
- Contribute to community blocklists

### 4. Geolocation Tracking
- Track bad actors by country/ASN
- Geographic blocking rules
- Regional attack patterns

### 5. Export Formats
- CSV export for analysis
- Firewall rule generation
- Integration with SIEM systems

### 6. Historical Analysis
- Time-series queries
- Attack pattern visualization
- Forensic investigation tools

### 7. Automatic Weight Tuning
- Adjust weights based on false positive rate
- A/B testing for chain effectiveness
- Feedback loop from manual reviews

### 8. Distributed Tracing
- Track IP journey across chains
- Visualize decision tree
- Debug complex scenarios

---

# Appendix E: FAQ

## General Questions

**Q: Why SQLite instead of PostgreSQL?**
A: Simpler deployment, no external dependencies, sufficient for scale, lower resource usage.

**Q: What happens if the database gets corrupted?**
A: Automatic detection on startup, backup corrupted DB, create fresh database, continue operation.

**Q: Can I still use the old journal/snapshot system?**
A: Yes, for 7 days after migration. Rollback procedure documented in Appendix B.

**Q: How much disk space will the database use?**
A: ~12.5 MB for 100k IPs, 400k events, 1000 bad actors. Scales linearly.

## Bad Actors Questions

**Q: How is bad actor scoring calculated?**
A: Sum of weights from all block events across all nodes. Each chain has a weight (0.0-1.0).

**Q: Can an IP be removed from bad actors?**
A: Yes, via `DELETE /ip/{ip}/clear` API endpoint.

**Q: What happens if an IP is in both good_actors and bad_actors?**
A: Good actors take priority. IP is removed from bad_actors and unblocked.

**Q: Is bad actor scoring per-website or global?**
A: Global across all websites. An IP malicious on one site is blocked on all.

**Q: How long are bad actors blocked?**
A: Permanently (until manual removal) with max duration from config (e.g., 1 week, renewed automatically).

## Cluster Questions

**Q: How do nodes sync bad actors?**
A: Events are synced across nodes, scores calculated from all events, promotion happens independently on each node.

**Q: What if nodes have different thresholds?**
A: Each node uses its own threshold. Bad actors are synced, so eventually all nodes converge.

**Q: How often do nodes sync?**
A: Incremental sync every 30 seconds (configurable).

**Q: What happens if a node is offline during sync?**
A: It catches up when it comes back online. Sync is idempotent.

## Performance Questions

**Q: How fast are bad actor checks?**
A: O(1) lookup on indexed TEXT primary key. Sub-millisecond.

**Q: Will this slow down log processing?**
A: No. Database operations are async. Blocking still works even if DB is slow.

**Q: How often is cleanup run?**
A: Every hour by default (configurable via `compaction_interval`).

**Q: What's the maximum database size?**
A: Practically unlimited. SQLite supports databases up to 281 TB.

## Migration Questions

**Q: Is migration automatic?**
A: Yes, on first startup after upgrade. Detects legacy files and migrates automatically.

**Q: Can I test migration before production?**
A: Yes, use `--dry-run` mode to test migration without affecting production:

```bash
# Test migration with a copy of production data
cp /var/lib/bot-detector/state/snapshot.json /tmp/test/
cp /var/lib/bot-detector/state/events.log /tmp/test/

# Run in dry-run mode (uses :memory: database)
./bot-detector --dry-run \
  --log-path /tmp/test_access.log \
  --config-dir /etc/bot-detector \
  --state-dir /tmp/test

# This will:
# 1. Load snapshot.json and events.log from /tmp/test
# 2. Migrate to SQLite in-memory database (:memory:)
# 3. Process test log file
# 4. Show statistics
# 5. Exit (database destroyed, legacy files unchanged)
```

**How :memory: database works:**
- SQLite creates database entirely in RAM
- No disk I/O (fast)
- Automatically destroyed on exit
- Same code path as production (full testing)
- Safe to test with production data copies

**What gets tested:**
- Migration logic (snapshot → SQLite)
- Bad actor scoring
- Database queries
- All SQLite operations

**What doesn't happen:**
- No database written to disk (`:memory:` only)
- No legacy files renamed (dry-run skips the `.migrated` rename)
- No actual blocking (dry-run mode)
- No persistence after exit

**Q: What if migration fails?**
A: Rollback procedure documented in Appendix B. Legacy files kept as `.migrated` for 7 days.

**Q: Will I lose data during migration?**
A: No. Migration is read-only on legacy files. Original files renamed only after success.

---

# Appendix F: Review Checklist

Quick at-a-glance status of all design areas.

## Schema Design
- [x] `reasons` - Hash-based IDs (FNV-1a), cluster-safe
- [x] `ips` - IP as TEXT primary key, cluster-safe
- [x] `ip_scores` - IP as TEXT primary key, temporary
- [x] `bad_actors` - IP as TEXT, permanent, with history_json
- [x] `events` - IP as TEXT, node_name, UNIQUE constraint for deduplication
- [x] `schema_version` - Migration tracking
- [x] All tables have appropriate indexes
- [x] No AUTOINCREMENT conflicts (IP as TEXT, hash-based reason IDs)

## Features
- [x] Weighted bad actor scoring (chain weights 0.0-1.0)
- [x] Configurable threshold and block duration
- [x] History JSON with node attribution
- [x] SQLite WAL mode for crash safety
- [x] State restoration on startup (blocked IPs, bad actors, unblocked IPs)
- [x] Migration from legacy journal/snapshot format
- [x] Event merging and incremental sync across cluster nodes
- [x] API endpoints (list, export, clear bad actors)
- [x] Metrics for SQLite operations and bad actors
- [x] Error handling (availability > persistence)
- [x] Database corruption detection and recovery
- [x] Health check endpoint
- [x] Graceful shutdown with WAL checkpoint
- [x] Dry-run mode with `:memory:` database
- [x] Config hot-reload (changes apply to new blocks only)

## Confirmed Unchanged
- ActivityStore (in-memory chain progress) — no SQLite interaction
- CleanUpIdleActors (actor cleanup routine) — independent of persistence
- ReasonCache (in-memory string interning) — kept alongside DB deduplication
- Good actors (config-based, not in DB) — priority over bad actors
- Multi-website support (works via reason strings, scoring is global)
