# SQLite Migration Plan

## Overview

Replace the journal/snapshot persistence system with SQLite. This is a clean break — no backward compatibility with the old format.

Bad actor scoring is built on top of this SQLite foundation (see [BAD_ACTORS.md](BAD_ACTORS.md)).

## Motivation

### Current System
- `state.snapshot` (gzipped JSON) + `events.log` (JSONL) + compaction logic
- Manual snapshot/journal management with replay on startup
- `PersistenceMutex` + `PersistenceWg` for async journal writes in goroutines
- Linear scans for queries (iterate full `IPStates` map)
- Compaction writes full snapshot + truncates journal periodically

### SQLite Benefits
- Single file (`state.db`), WAL mode for crash safety
- Indexed queries, no linear scans
- No compaction logic — cleanup is just `DELETE` + periodic `VACUUM`
- Synchronous writes (WAL mode is fast enough, eliminates async goroutine complexity)
- `PersistenceWg` and `JournalHandle` eliminated entirely
- Rich queries for bad actors, analytics

## Architecture

### Single Database File
**Location:** `{state_dir}/state.db`

**Mode:** WAL (Write-Ahead Logging)
- Crash-safe transactions
- Better read/write concurrency
- Automatic checkpointing

### Schema

```sql
CREATE TABLE schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);

CREATE TABLE reasons (
    id INTEGER PRIMARY KEY,  -- FNV-1a 64-bit hash of reason text
    reason TEXT UNIQUE NOT NULL
);
CREATE INDEX idx_reason_text ON reasons(reason);

CREATE TABLE ips (
    ip TEXT PRIMARY KEY,
    state TEXT CHECK(state IN ('blocked', 'unblocked')),
    expire_time TIMESTAMP,
    reason_id INTEGER REFERENCES reasons(id),
    modified_at TIMESTAMP,
    first_blocked_at TIMESTAMP
);
CREATE INDEX idx_ips_state ON ips(state);
CREATE INDEX idx_ips_expire_time ON ips(expire_time);

CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TIMESTAMP NOT NULL,
    event_type TEXT NOT NULL CHECK(event_type IN ('block', 'unblock')),
    ip TEXT NOT NULL,
    reason_id INTEGER REFERENCES reasons(id),
    duration INTEGER,  -- nanoseconds, for block events
    node_name TEXT,
    UNIQUE(timestamp, ip, node_name, event_type)
);
CREATE INDEX idx_events_ip_timestamp ON events(ip, timestamp);
CREATE INDEX idx_events_timestamp ON events(timestamp);
```

### Key Design Decisions

- **IP as TEXT primary key** — no AUTOINCREMENT conflicts across cluster nodes, natural key
- **Reason ID as FNV-1a 64-bit hash** — deterministic, cluster-safe, no coordination needed
- **Events table** — replaces `events.log` journal, queryable, with UNIQUE constraint for dedup during cluster sync
- **Schema versioning** — `schema_version` table with migration hooks (v1: core tables, v2: bad actors tables)

### Dry-Run Mode

Uses `:memory:` SQLite database — same code path as production, no disk I/O, automatic cleanup on exit.

## What Changes

### Processor Fields (internal/app/types.go)

**Removed:**
- `PersistenceWg sync.WaitGroup` — no more async writes
- `JournalHandle *os.File` — no more journal file
- `IPStates map[string]persistence.IPState` — replaced by SQLite queries

**Added:**
- `DB *sql.DB` — SQLite database handle

**Kept (behavior changes):**
- `PersistenceMutex sync.Mutex` — still used to serialize SQLite writes and satisfy `GetPersistenceMutex()` interface
- `PersistenceEnabled bool` — same semantics
- `StateDir string` — same semantics
- `CompactionInterval time.Duration` — repurposed for cleanup interval

### Files Modified

| File | What Changes |
|------|-------------|
| `internal/persistence/io.go` | **Delete entirely** — replaced by sqlite.go |
| `internal/persistence/io_test.go` | **Delete entirely** |
| `internal/persistence/production_test.go` | **Delete entirely** |
| `internal/persistence/sqlite.go` | **New** — all SQLite operations |
| `internal/persistence/sqlite_test.go` | **New** — tests |
| `internal/persistence/migrate.go` | **New** — one-time migration from legacy format |
| `internal/persistence/migrate_test.go` | **New** — migration tests |
| `internal/app/types.go` | Remove `PersistenceWg`, `JournalHandle`, `IPStates`; add `DB *sql.DB` |
| `internal/app/providers.go` | `GetIPStates()` queries SQLite; `GetPersistenceState()` queries SQLite; `RemoveFromPersistence()` deletes from SQLite |
| `internal/app/configmanager.go` | `unblockNewlyWhitelistedIPs()` — query SQLite instead of iterating `p.IPStates` |
| `internal/checker/checker.go` | `executeBlock()` — synchronous SQLite insert, no goroutine; `CheckChains()` unblock path — SQLite query/insert |
| `cmd/bot-detector/main.go` | `initializeProcessor()`, `restorePersistenceState()`, `runCompaction()` → `runCleanup()`, `performGracefulShutdown()`, `fetchInitialStateFromCluster()`, `replayJournalAfter()`, resync callback |
| `cmd/bot-detector/main_test.go` | Update `TestCompaction` → `TestCleanup` |
| `cmd/bot-detector/race_test.go` | Replace `p.IPStates` map access with SQLite calls |
| `cmd/bot-detector/errors_test.go` | Corrupted snapshot → corrupted DB |
| `internal/cluster/syncloop.go` | `StateSyncManager` — replace map field with DB queries |

### Files NOT Modified

- `internal/persistence/types.go` — keep all types (`IPState`, `BlockState`, `AuditEvent`, etc.)
- `internal/store/store.go` — ActivityStore is in-memory, independent
- `internal/blocker/*` — blocker interface unchanged
- `internal/config/*` — no config changes in this phase
- `internal/metrics/metrics.go` — no new metrics in this phase
- `internal/server/types.go` — Provider interface unchanged
- `internal/server/handlers_ip.go` — calls Provider methods, unchanged
- `internal/server/handlers_statesync.go` — calls `GetIPStates()`, unchanged
- `internal/server/server.go` — route registration unchanged
- `internal/server/*_test.go` — mocks return `map[string]persistence.IPState{}`, still valid
- `internal/logparser/*`, `internal/parser/*` — unrelated
- `internal/app/processor.go` — InternReason stays as-is

## SQLite Operations (persistence/sqlite.go)

### Core Functions

```go
// Database lifecycle
func OpenDB(stateDir string, dryRun bool) (*sql.DB, error)
func ApplyMigrations(db *sql.DB) error
func CloseDB(db *sql.DB) error  // WAL checkpoint + close

// Reason deduplication
func GetOrCreateReasonID(db *sql.DB, reason string) (int64, error)

// IP state operations
func UpsertIPState(db *sql.DB, ip string, state BlockState, expireTime time.Time, reason string, modifiedAt time.Time, firstBlockedAt time.Time) error
func GetIPState(db *sql.DB, ip string) (*IPState, error)
func DeleteIPState(db *sql.DB, ip string) error
func GetAllIPStates(db *sql.DB) (map[string]IPState, error)
func GetBlockedIPs(db *sql.DB) (map[string]IPState, error)

// Event operations
func InsertEvent(db *sql.DB, timestamp time.Time, eventType EventType, ip string, reason string, duration time.Duration, nodeName string) error

// Cleanup
func CleanupExpiredBlocks(db *sql.DB, now time.Time) (int, error)
func CleanupOldUnblocked(db *sql.DB, now time.Time, retentionPeriod time.Duration) (int, error)
func CleanupOldEvents(db *sql.DB, retentionPeriod time.Duration) (int, error)
func CleanupOrphanedReasons(db *sql.DB) (int, error)
```

### GetAllIPStates (Provider compatibility)

This is the key function that keeps the `server.Provider` interface working without changes:

```go
func GetAllIPStates(db *sql.DB) (map[string]IPState, error) {
    rows, err := db.Query(`
        SELECT i.ip, i.state, i.expire_time, r.reason, i.modified_at, i.first_blocked_at
        FROM ips i LEFT JOIN reasons r ON r.id = i.reason_id
    `)
    // ... build and return map[string]IPState
}
```

Called by `GetIPStates()` in providers.go. Used by state sync handlers (every 30s) and resync callback (on backend recovery). Acceptable performance for these periodic operations.

## Migration from Legacy Format (persistence/migrate.go)

On startup, if `state.snapshot` or `events.log` exist in the state directory and `state.db` does not:
1. Load snapshot → insert into `ips` table
2. Replay journal → insert into `events` table, update `ips` table
3. Rename legacy files to `.migrated` (keeps rollback option)

```go
func MigrateFromLegacy(db *sql.DB, stateDir string) error
func ShouldMigrate(stateDir string) bool
```

If `state.db` already exists, skip migration entirely.

### Rollback

To roll back to the old version after migration:
1. Stop bot-detector
2. Remove `state.db`, `state.db-wal`, `state.db-shm`
3. Rename `state.snapshot.migrated` → `state.snapshot`, `events.log.migrated` → `events.log`
4. Deploy old binary, start

The `.migrated` files can be manually deleted once the upgrade is confirmed stable.

### Cluster Upgrade Path

The cluster sync wire format (`map[string]IPState` as JSON) is unchanged — the Provider interface returns the same types regardless of backend. This means:

1. **Upgrade leader first** — it migrates to SQLite, serves identical JSON to followers
2. **Old followers continue working** — they fetch the same `StateSyncResponse` format from the leader
3. **Upgrade followers** — each migrates its own local state to SQLite on first start
4. No coordination needed, no protocol version negotiation

## Cluster State Sync

### Wire Format: Unchanged

The cluster sync protocol stays the same:
- `GET /api/v1/cluster/internal/persistence/state` returns `StateSyncResponse{States: map[string]IPState}`
- `GET /api/v1/cluster/state/merged` returns `MergedStateResponse{States: map[string]IPState}`

The handlers call `p.GetIPStates()` which now queries SQLite internally. Followers receive the same JSON and apply it to their local SQLite.

### StateSyncManager Changes

`StateSyncManager` currently holds `ipStates map[string]persistence.IPState` and `ipStatesMutex *sync.Mutex`. These change to query through the DB. The sync loop that applies received states writes to SQLite via `UpsertIPState()`.

### fetchInitialStateFromCluster Changes

Currently writes `p.IPStates = states`. Will instead iterate the received map and call `UpsertIPState()` for each entry in a transaction.

### replayJournalAfter Changes

Currently reads `events.log` and writes to `p.IPStates`. Will instead call `InsertEvent()` + `UpsertIPState()`. Only needed during migration — after migration, the journal file won't exist.

## State Restoration on Startup

### Current Flow
1. Load `state.snapshot` → `p.IPStates`
2. Replay `events.log` → update `p.IPStates`
3. Query HAProxy current state
4. For each IP in `p.IPStates`: restore to HAProxy
5. Open journal for appending

### New Flow
1. Open/create `state.db` (runs migrations)
2. If legacy files exist and DB is empty: run migration
3. Query `ips` table for blocked/unblocked IPs
4. For each IP: restore to HAProxy — same logic as today
5. Done (no journal to open)

## Compaction → Cleanup

### Current: `runCompaction()`
1. Lock mutex
2. Delete expired blocks from `p.IPStates` map
3. Delete old unblocked entries
4. Write full snapshot to disk
5. Truncate journal
6. Unlock

### New: `runCleanup()`
1. `DELETE FROM ips WHERE state = 'blocked' AND expire_time < ?` (now)
2. `DELETE FROM ips WHERE state = 'unblocked' AND expire_time < ?` (now - retention)
3. `DELETE FROM events WHERE timestamp < ?` (now - retention)
4. `DELETE FROM reasons WHERE id NOT IN (SELECT reason_id FROM ips UNION SELECT reason_id FROM events)`
5. Log stats

No snapshot, no journal truncation. Periodic `VACUUM` can run weekly.

## Graceful Shutdown

### Current
1. `p.PersistenceWg.Wait()` — wait for async journal writes
2. `runCompaction(p)` — final snapshot
3. `p.JournalHandle.Close()`

### New
1. `CloseDB(p.DB)` — WAL checkpoint + close

## Error Handling

**Priority: Availability > Persistence**

- If DB open fails on startup: log error, set `PersistenceEnabled = false`, continue in-memory only
- If a write fails during operation: log warning, skip persistence for that event, continue processing
- If a read fails: log warning, return empty/default, continue processing
- Blocking still works via HAProxy even if persistence is down

## Implementation Tasks

### Task 1: Add SQLite dependency and create persistence/sqlite.go

**What:**
- Add `modernc.org/sqlite` to `go.mod`
- Create `internal/persistence/sqlite.go` with all functions listed in "SQLite Operations" above
- Create `internal/persistence/sqlite_test.go`

**Does NOT touch:** Any existing file. Purely additive.

**Tests:**
- DB opens in WAL mode
- Schema created correctly (all tables, indexes)
- Migration system is idempotent (run twice = no error)
- `GetOrCreateReasonID` deduplication (same reason → same hash)
- `UpsertIPState` insert and update
- `GetIPState` returns correct data with reason text via JOIN
- `GetAllIPStates` returns full map matching `map[string]IPState` shape
- `GetBlockedIPs` returns only non-expired blocked IPs
- `InsertEvent` with UNIQUE constraint (duplicate = no error via INSERT OR IGNORE)
- All cleanup functions delete correct rows, keep others
- `CloseDB` performs WAL checkpoint
- `:memory:` mode works

**Acceptance:** All new tests pass with `-race`. No existing tests affected.

---

### Task 2: Create persistence/migrate.go

**What:**
- Create `internal/persistence/migrate.go` with `MigrateFromLegacy()` and `ShouldMigrate()`
- Create `internal/persistence/migrate_test.go`
- Uses existing `LoadSnapshot()` and journal parsing from `io.go` (don't delete io.go yet — Task 4)
- Renames legacy files to `.migrated` after successful migration (not delete)

**Does NOT touch:** Any existing file except reading/renaming.

**Tests:**
- Migration from snapshot with known data → verify all IPs in `ips` table
- Migration from journal with known events → verify events in `events` table and `ips` table updated
- Legacy files renamed to `.migrated` after success
- Migration skipped if `state.db` already has data
- Migration skipped if no legacy files exist
- Uses `testdata/v1/state.snapshot` and `testdata/v1/events.log` for real format testing

**Acceptance:** Migration produces correct SQLite state from legacy files. Legacy files preserved as `.migrated`. All tests pass with `-race`.

---

### Task 3: Wire SQLite into Processor and replace IPStates map

The core switchover. Touches many files but each change is mechanical — replacing map access with function calls.

**`internal/app/types.go`:**
- Remove: `PersistenceWg sync.WaitGroup`, `JournalHandle *os.File`, `IPStates map[string]persistence.IPState`
- Add: `DB *sql.DB`

**`internal/app/providers.go`:**
- `GetPersistenceState(ip)` → `persistence.GetIPState(p.DB, ip)`
- `RemoveFromPersistence(ip)` → `persistence.DeleteIPState(p.DB, ip)` + `persistence.InsertEvent(...)` for audit
- `GetIPStates()` → `persistence.GetAllIPStates(p.DB)`
- `GetPersistenceMutex()` → unchanged (returns `&p.PersistenceMutex`)

**`internal/checker/checker.go`:**
- `executeBlock()`:
  - Remove goroutine + `PersistenceWg.Add/Done`
  - Synchronous: `persistence.InsertEvent(p.DB, ...)` + `persistence.UpsertIPState(p.DB, ...)`
  - Keep `PersistenceMutex.Lock/Unlock` around the writes
- `CheckChains()` unblock-on-good-actor path:
  - Replace `p.IPStates[ip]` read with `persistence.GetIPState(p.DB, ip)`
  - Replace journal write + map update with `persistence.InsertEvent()` + `persistence.UpsertIPState()`

**`internal/app/configmanager.go`:**
- `unblockNewlyWhitelistedIPs()`:
  - Replace `for ip, state := range p.IPStates` with `persistence.GetBlockedIPs(p.DB)` query
  - Replace `p.IPStates[ip] = ...` with `persistence.UpsertIPState(p.DB, ...)`

**`cmd/bot-detector/main.go`:**
- `initializeProcessor()`: remove `IPStates`/`JournalHandle` init, `DB` field set to nil (opened later)
- `restorePersistenceState()`: rewrite — open DB, run migration if needed, query IPs, restore to HAProxy
- `fetchInitialStateFromCluster()`: replace `p.IPStates = states` with transaction of `UpsertIPState()` calls
- `replayJournalAfter()`: replace map writes with `UpsertIPState()` + `InsertEvent()` calls
- `runCompaction()` → `runCleanup()`: replace snapshot+journal with cleanup SQL
- `performGracefulShutdown()`: replace `PersistenceWg.Wait()` + compaction + journal close with `CloseDB()`
- Resync callback: replace `for ip, state := range p.IPStates` with `persistence.GetAllIPStates(p.DB)`

**`internal/cluster/syncloop.go`:**
- `StateSyncManager`: replace `ipStates` map field with `db *sql.DB` (or provider function)
- Sync apply: write received states to SQLite via `UpsertIPState()`

**Tests to update:**
- `cmd/bot-detector/main_test.go` — `TestCompaction` → `TestCleanup`, use SQLite
- `cmd/bot-detector/race_test.go` — replace `p.IPStates` map access with SQLite calls
- `cmd/bot-detector/errors_test.go` — corrupted snapshot → corrupted DB

**Acceptance:** All existing tests pass with `-race`. Block/unblock round-trips through SQLite. State restoration works. Cluster sync works. Cleanup works.

---

### Task 4: Delete legacy persistence code

**What:**
- Delete `internal/persistence/io.go`
- Delete `internal/persistence/io_test.go`
- Delete `internal/persistence/production_test.go`
- Inline the minimal snapshot/journal reading needed by `migrate.go` (or keep a small `legacy_reader.go` if cleaner)
- Remove any remaining references to `LoadSnapshot`, `WriteSnapshot`, `OpenJournalForAppend`, `WriteEventToJournal`, `GetJournalPath`

**Acceptance:** `go build ./...` succeeds. `go test -race ./...` passes. No references to old persistence functions remain except in migration code.

---

### Task 5: Update documentation

**What:**
- Update `README.md` — mention SQLite, remove journal/snapshot references
- Update `docs/Persistence.md` — rewrite for SQLite
- Update `docs/ClusterStateSync.md` if needed

**Acceptance:** Documentation is accurate and consistent.

## Task Dependency Graph

```
Task 1 (sqlite.go)       ← purely additive, no breakage
    ↓
Task 2 (migrate.go)      ← purely additive, uses io.go to read legacy files
    ↓
Task 3 (wire into Processor)  ← the breaking change
    ↓
Task 4 (delete legacy code)   ← cleanup
    ↓
Task 5 (docs)
```

## Touchpoint Summary

Every place in the codebase that accesses `p.IPStates`, `p.JournalHandle`, or `p.PersistenceWg`:

| Location | Current | New |
|----------|---------|-----|
| `checker.go:executeBlock()` | goroutine + `PersistenceWg` + `WriteEventToJournal` + `p.IPStates[ip] = ...` | synchronous `InsertEvent()` + `UpsertIPState()` |
| `checker.go:CheckChains()` unblock path | `p.IPStates[ip]` read + `WriteEventToJournal` + `p.IPStates[ip] = ...` | `GetIPState()` + `InsertEvent()` + `UpsertIPState()` |
| `configmanager.go:unblockNewlyWhitelistedIPs()` | `for ip, state := range p.IPStates` | `GetBlockedIPs()` query |
| `providers.go:GetPersistenceState()` | `p.IPStates[ip]` | `GetIPState()` |
| `providers.go:RemoveFromPersistence()` | `delete(p.IPStates, ip)` + `WriteEventToJournal` | `DeleteIPState()` + `InsertEvent()` |
| `providers.go:GetIPStates()` | `return p.IPStates` | `GetAllIPStates()` |
| `main.go:initializeProcessor()` | `IPStates: make(...)` | `DB: nil` (opened later) |
| `main.go:restorePersistenceState()` | load snapshot + replay journal + open journal | open DB + migrate + query |
| `main.go:fetchInitialStateFromCluster()` | `p.IPStates = states` | transaction of `UpsertIPState()` |
| `main.go:replayJournalAfter()` | `p.IPStates[ip] = ...` | `UpsertIPState()` + `InsertEvent()` |
| `main.go:runCompaction()` | snapshot write + journal truncate | `CleanupExpiredBlocks()` + `CleanupOldUnblocked()` + `CleanupOldEvents()` + `CleanupOrphanedReasons()` |
| `main.go:performGracefulShutdown()` | `PersistenceWg.Wait()` + compaction + `JournalHandle.Close()` | `CloseDB()` |
| `main.go:resyncCallback` | `for ip, state := range p.IPStates` | `GetAllIPStates()` or `GetBlockedIPs()` |
| `syncloop.go:StateSyncManager` | holds `ipStates` map | queries DB |
| `race_test.go` | concurrent `p.IPStates` access | concurrent SQLite access |
| `main_test.go:TestCompaction` | snapshot + journal assertions | cleanup assertions |
| `errors_test.go` | corrupted snapshot | corrupted DB |

## Components That Remain Unchanged

- **ActivityStore** (in-memory chain progress) — no SQLite interaction
- **CleanUpIdleActors** (actor cleanup routine) — independent of persistence
- **ReasonCache / InternReason** (in-memory string interning) — kept alongside DB deduplication, applied on DB read paths too
- **Good actors** (config-based, not in DB)
- **Multi-website support** (works via reason strings)
- **server.Provider interface** — same methods, same return types
- **All server test mocks** — return `map[string]persistence.IPState{}`, still valid
- **Blocker interface** — unchanged
- **Config types and parsing** — unchanged
