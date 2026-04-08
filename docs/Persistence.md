# Bot-Detector Persistence and State Management

This document describes how `bot-detector` persists its state using SQLite.

## Enabling Persistence

Persistence is controlled by the `--state-dir` command-line flag. When you specify a state directory, persistence is **automatically enabled** unless explicitly disabled in the configuration.

```sh
./bot-detector --config-dir /etc/bot-detector --state-dir /var/lib/bot-detector/state
```

To disable persistence even when `--state-dir` is provided:

```yaml
application:
  persistence:
    enabled: false
```

You can also configure the cleanup interval and retention period:

```yaml
application:
  persistence:
    compaction_interval: "1h"  # How often to clean up expired entries (default: 1h)
    retention_period: "168h"   # How long to keep unblocked entries (default: 168h / 1 week)
```

## Database

Persistence uses a single SQLite database file: `{state_dir}/state.db`

The database runs in **WAL (Write-Ahead Logging) mode** for crash safety and better read/write concurrency.

### Schema

**ips** — Current state per IP address:
- `ip` (TEXT PRIMARY KEY) — IP address
- `state` — "blocked" or "unblocked"
- `expire_time` — When the block expires
- `reason_id` — Foreign key to reasons table
- `modified_at` — Last modification timestamp
- `first_blocked_at` — Earliest block timestamp (preserved across re-blocks)

**events** — Audit log of all block/unblock actions:
- `timestamp`, `event_type`, `ip`, `reason_id`, `duration`, `node_name`
- UNIQUE constraint on `(timestamp, ip, node_name, event_type)` for cluster deduplication

**reasons** — Deduplicated reason strings with FNV-1a hash-based IDs (cluster-safe, deterministic).

**schema_version** — Tracks applied migrations for future schema upgrades.

## Guiding Principles

- **Local State is Truth:** The SQLite database is the source of truth. The backend's (e.g., HAProxy) state is a replica synchronized on startup.
- **Availability > Persistence:** If the database fails during operation, blocking continues via HAProxy. Persistence errors are logged but don't halt processing.
- **Synchronous Writes:** All state changes are written to SQLite synchronously (WAL mode makes this fast). No async goroutines or write queues.
- **Unified State:** Both blocked IPs (`gpc0=1`) and explicitly unblocked IPs (`gpc0=0`) are tracked for complete state restoration.

## Key Processes

### Startup and State Restoration

1. **Open Database:** Open or create `state.db`, apply any pending schema migrations.
2. **Legacy Migration:** If `state.snapshot` or `events.log` exist and the database is empty, migrate the legacy data into SQLite and rename the old files to `.migrated`.
3. **Query State:** Read all IP states from the `ips` table.
4. **State Push:** Issue block/unblock commands to HAProxy for each IP, respecting good actor configuration and checking for already-synced entries.

### Runtime

Every block or unblock action:
1. Insert event into `events` table (audit trail)
2. Upsert IP state in `ips` table
3. Queue command for HAProxy backend (rate-limited)

All database writes are synchronous and protected by a mutex.

### Periodic Cleanup

A background goroutine runs at the configured `compaction_interval` (default: 1 hour):

1. Delete expired blocks from `ips` table
2. Delete old unblocked entries past the retention period
3. Delete old events past the retention period
4. Delete orphaned reason strings

No snapshots, no journal truncation — just SQL DELETE queries.

### Graceful Shutdown

1. WAL checkpoint (flush WAL to main database file)
2. Close database connection

## Dry-Run Mode

In dry-run mode, the database is created in memory (`:memory:`). This runs the exact same code path as production but with no disk I/O. The database is automatically destroyed on exit.

## Cluster Support

The cluster state sync protocol is unchanged — nodes exchange `map[string]IPState` as JSON over HTTP. The SQLite database is queried to produce this map on the leader side, and received states are written to SQLite on the follower side.

See [ClusterStateSync.md](ClusterStateSync.md) for details.

## Migration from Legacy Format

When upgrading from the old snapshot/journal persistence:

1. Start the new version with the same `--state-dir`
2. On first startup, the application detects `state.snapshot` and/or `events.log`
3. Data is imported into SQLite (snapshot first, then journal replay)
4. Legacy files are renamed to `state.snapshot.migrated` and `events.log.migrated`
5. Normal operation continues with SQLite

### Rollback

To revert to the old version:
1. Stop bot-detector
2. Remove `state.db`, `state.db-wal`, `state.db-shm`
3. Rename `state.snapshot.migrated` → `state.snapshot` and `events.log.migrated` → `events.log`
4. Deploy old binary and start

## Failure Scenarios

| Failure Type | Behavior | Outcome |
|:---|:---|:---|
| **App crash** | SQLite WAL ensures committed transactions survive. On restart, database is intact. | **Resilient.** No data loss. |
| **Database corruption** | Detected on open. Application logs error and can continue without persistence. | **Degraded.** Blocking still works via HAProxy. |
| **Disk full** | Write operations fail. Errors logged, processing continues. | **Degraded.** Events not persisted until space freed. |
| **HAProxy restart** | Full state (blocks + unblocks) restored from database on next bot-detector startup. | **Resilient.** |

## Database Maintenance

### Manual Inspection
```bash
sqlite3 /var/lib/bot-detector/state/state.db <<EOF
SELECT 'IPs:', COUNT(*) FROM ips;
SELECT 'Events:', COUNT(*) FROM events;
SELECT 'Reasons:', COUNT(*) FROM reasons;
SELECT 'DB Size:', page_count * page_size / 1024 / 1024 || ' MB'
FROM pragma_page_count(), pragma_page_size();
EOF
```

### Manual Vacuum
```bash
sqlite3 /var/lib/bot-detector/state/state.db "VACUUM"
```

### Backup
```bash
sqlite3 /var/lib/bot-detector/state/state.db ".backup /backup/state.db.$(date +%Y%m%d)"
```

## Logging Output

### Startup
```
SETUP: Persistence enabled. Loading state from '/var/lib/bot-detector/state'...
STATE_LOAD: Loaded 105 IP states from database (100 blocked + 5 unblocked)
STATE_RESTORE: State restoration complete: 95 restored, 10 skipped (already_blocked=3, already_unblocked=2, expired=4, good_actors=1)
```

### Migration (first run after upgrade)
```
MIGRATION: Legacy persistence files detected, migrating to SQLite...
MIGRATION: Legacy persistence migration completed successfully
```

### Cleanup
```
CLEANUP: Cleanup completed: expired_blocks=15, old_unblocked=3, old_events=1200, orphaned_reasons=5
```

### Shutdown
```
SHUTDOWN: Performing final cleanup...
PERSISTENCE: Closing database.
SHUTDOWN: Shutdown complete.
```
