package persistence

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the current database schema version.
const SchemaVersion = 1

// OpenDB opens (or creates) the SQLite database with WAL mode.
// In dry-run mode, uses an in-memory database.
func OpenDB(stateDir string, dryRun bool) (*sql.DB, error) {
	var dsn string
	if dryRun {
		dsn = ":memory:"
	} else {
		dsn = filepath.Join(stateDir, "state.db")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Set pragmas explicitly (DSN parameters not supported by modernc.org/sqlite)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to set pragma %q: %w", p, err)
		}
	}

	if err := ApplyMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	return db, nil
}

// CloseDB performs a WAL checkpoint and closes the database.
func CloseDB(db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return db.Close()
}

// ApplyMigrations creates or upgrades the database schema.
func ApplyMigrations(db *sql.DB) error {
	// Create schema_version table if it doesn't exist
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		description TEXT
	)`)
	if err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	var currentVersion int
	err = db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("failed to get current schema version: %w", err)
	}

	if currentVersion < 1 {
		if err := migrateV1(db); err != nil {
			return fmt.Errorf("migration v1 failed: %w", err)
		}
	}

	return nil
}

func migrateV1(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE reasons (
			id INTEGER PRIMARY KEY,
			reason TEXT UNIQUE NOT NULL
		)`,
		`CREATE INDEX idx_reason_text ON reasons(reason)`,

		`CREATE TABLE ips (
			ip TEXT PRIMARY KEY,
			state TEXT CHECK(state IN ('blocked', 'unblocked')),
			expire_time TIMESTAMP,
			reason_id INTEGER REFERENCES reasons(id),
			modified_at TIMESTAMP,
			first_blocked_at TIMESTAMP
		)`,
		`CREATE INDEX idx_ips_state ON ips(state)`,
		`CREATE INDEX idx_ips_expire_time ON ips(expire_time)`,

		`CREATE TABLE events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TIMESTAMP NOT NULL,
			event_type TEXT NOT NULL CHECK(event_type IN ('block', 'unblock')),
			ip TEXT NOT NULL,
			reason_id INTEGER REFERENCES reasons(id),
			duration INTEGER,
			node_name TEXT,
			UNIQUE(timestamp, ip, node_name, event_type)
		)`,
		`CREATE INDEX idx_events_ip_timestamp ON events(ip, timestamp)`,
		`CREATE INDEX idx_events_timestamp ON events(timestamp)`,

		`INSERT INTO schema_version (version, description) VALUES (1, 'Initial schema')`,
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("failed to execute %q: %w", stmt[:min(len(stmt), 60)], err)
		}
	}

	return tx.Commit()
}

// reasonHash returns a deterministic FNV-1a 64-bit hash for a reason string.
func reasonHash(reason string) int64 {
	h := fnv.New64a()
	h.Write([]byte(reason))
	return int64(h.Sum64())
}

// GetOrCreateReasonID returns the hash-based ID for a reason, inserting if needed.
func GetOrCreateReasonID(db *sql.DB, reason string) (int64, error) {
	id := reasonHash(reason)
	_, err := db.Exec("INSERT OR IGNORE INTO reasons (id, reason) VALUES (?, ?)", id, reason)
	if err != nil {
		return 0, fmt.Errorf("failed to insert reason: %w", err)
	}
	return id, nil
}

// UpsertIPState inserts or updates an IP's state.
func UpsertIPState(db *sql.DB, ip string, state BlockState, expireTime time.Time, reason string, modifiedAt time.Time, firstBlockedAt time.Time) error {
	reasonID, err := GetOrCreateReasonID(db, reason)
	if err != nil {
		return err
	}

	_, err = db.Exec(`INSERT INTO ips (ip, state, expire_time, reason_id, modified_at, first_blocked_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET
			state = excluded.state,
			expire_time = excluded.expire_time,
			reason_id = excluded.reason_id,
			modified_at = excluded.modified_at,
			first_blocked_at = CASE
				WHEN ips.first_blocked_at IS NOT NULL AND ips.first_blocked_at != '' AND ips.first_blocked_at < excluded.first_blocked_at
				THEN ips.first_blocked_at
				ELSE excluded.first_blocked_at
			END`,
		ip, state.String(), expireTime, reasonID, modifiedAt, firstBlockedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert IP state: %w", err)
	}
	return nil
}

// GetIPState returns the state for a single IP, or nil if not found.
func GetIPState(db *sql.DB, ip string) (*IPState, error) {
	var stateStr string
	var expireTime, modifiedAt, firstBlockedAt sql.NullTime
	var reason sql.NullString

	err := db.QueryRow(`
		SELECT i.state, i.expire_time, r.reason, i.modified_at, i.first_blocked_at
		FROM ips i LEFT JOIN reasons r ON r.id = i.reason_id
		WHERE i.ip = ?`, ip).Scan(&stateStr, &expireTime, &reason, &modifiedAt, &firstBlockedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query IP state: %w", err)
	}

	return &IPState{
		State:          parseBlockState(stateStr),
		ExpireTime:     nullTimeValue(expireTime),
		Reason:         nullStringValue(reason),
		ModifiedAt:     nullTimeValue(modifiedAt),
		FirstBlockedAt: nullTimeValue(firstBlockedAt),
	}, nil
}

// DeleteIPState removes an IP from the ips table.
func DeleteIPState(db *sql.DB, ip string) error {
	_, err := db.Exec("DELETE FROM ips WHERE ip = ?", ip)
	if err != nil {
		return fmt.Errorf("failed to delete IP state: %w", err)
	}
	return nil
}

// GetAllIPStates returns all IP states as a map, matching the Provider interface.
func GetAllIPStates(db *sql.DB) (map[string]IPState, error) {
	rows, err := db.Query(`
		SELECT i.ip, i.state, i.expire_time, r.reason, i.modified_at, i.first_blocked_at
		FROM ips i LEFT JOIN reasons r ON r.id = i.reason_id`)
	if err != nil {
		return nil, fmt.Errorf("failed to query IP states: %w", err)
	}
	defer rows.Close()

	return scanIPStates(rows)
}

// GetBlockedIPs returns only blocked, non-expired IPs.
func GetBlockedIPs(db *sql.DB, now time.Time) (map[string]IPState, error) {
	rows, err := db.Query(`
		SELECT i.ip, i.state, i.expire_time, r.reason, i.modified_at, i.first_blocked_at
		FROM ips i LEFT JOIN reasons r ON r.id = i.reason_id
		WHERE i.state = 'blocked' AND i.expire_time > ?`, now)
	if err != nil {
		return nil, fmt.Errorf("failed to query blocked IPs: %w", err)
	}
	defer rows.Close()

	return scanIPStates(rows)
}

// InsertEvent records a block or unblock event.
func InsertEvent(db *sql.DB, timestamp time.Time, eventType EventType, ip string, reason string, duration time.Duration, nodeName string) error {
	reasonID, err := GetOrCreateReasonID(db, reason)
	if err != nil {
		return err
	}

	var durNanos *int64
	if duration > 0 {
		d := int64(duration)
		durNanos = &d
	}

	var nodeNamePtr *string
	if nodeName != "" {
		nodeNamePtr = &nodeName
	}

	_, err = db.Exec(`INSERT OR IGNORE INTO events (timestamp, event_type, ip, reason_id, duration, node_name)
		VALUES (?, ?, ?, ?, ?, ?)`,
		timestamp, string(eventType), ip, reasonID, durNanos, nodeNamePtr)
	if err != nil {
		return fmt.Errorf("failed to insert event: %w", err)
	}
	return nil
}

// CleanupExpiredBlocks removes blocked IPs whose expire_time has passed.
func CleanupExpiredBlocks(db *sql.DB, now time.Time) (int, error) {
	res, err := db.Exec("DELETE FROM ips WHERE state = 'blocked' AND expire_time < ?", now)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup expired blocks: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CleanupOldUnblocked removes unblocked IPs older than the retention period.
func CleanupOldUnblocked(db *sql.DB, now time.Time, retentionPeriod time.Duration) (int, error) {
	cutoff := now.Add(-retentionPeriod)
	res, err := db.Exec("DELETE FROM ips WHERE state = 'unblocked' AND expire_time < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old unblocked: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CleanupOldEvents removes events older than the retention period.
func CleanupOldEvents(db *sql.DB, retentionPeriod time.Duration) (int, error) {
	cutoff := time.Now().Add(-retentionPeriod)
	res, err := db.Exec("DELETE FROM events WHERE timestamp < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old events: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CleanupOrphanedReasons removes reasons not referenced by any IP or event.
func CleanupOrphanedReasons(db *sql.DB) (int, error) {
	res, err := db.Exec(`DELETE FROM reasons WHERE id NOT IN (
		SELECT reason_id FROM ips WHERE reason_id IS NOT NULL
		UNION
		SELECT reason_id FROM events WHERE reason_id IS NOT NULL
	)`)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup orphaned reasons: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- helpers ---

func scanIPStates(rows *sql.Rows) (map[string]IPState, error) {
	states := make(map[string]IPState)
	for rows.Next() {
		var ip, stateStr string
		var expireTime, modifiedAt, firstBlockedAt sql.NullTime
		var reason sql.NullString

		if err := rows.Scan(&ip, &stateStr, &expireTime, &reason, &modifiedAt, &firstBlockedAt); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		states[ip] = IPState{
			State:          parseBlockState(stateStr),
			ExpireTime:     nullTimeValue(expireTime),
			Reason:         nullStringValue(reason),
			ModifiedAt:     nullTimeValue(modifiedAt),
			FirstBlockedAt: nullTimeValue(firstBlockedAt),
		}
	}
	return states, rows.Err()
}

func parseBlockState(s string) BlockState {
	if s == "blocked" {
		return BlockStateBlocked
	}
	return BlockStateUnblocked
}

func nullTimeValue(t sql.NullTime) time.Time {
	if t.Valid {
		return t.Time
	}
	return time.Time{}
}

func nullStringValue(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}
