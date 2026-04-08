package persistence

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the current database schema version.
const SchemaVersion = 2

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

	if currentVersion < 2 {
		if err := migrateV2(db); err != nil {
			return fmt.Errorf("migration v2 failed: %w", err)
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

func migrateV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE ip_scores (
			ip TEXT PRIMARY KEY,
			score REAL NOT NULL DEFAULT 0.0,
			block_count INTEGER NOT NULL DEFAULT 0,
			last_block_time TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX idx_ip_scores_score ON ip_scores(score)`,
		`CREATE INDEX idx_ip_scores_last_block_time ON ip_scores(last_block_time)`,

		`CREATE TABLE bad_actors (
			ip TEXT PRIMARY KEY,
			promoted_at TIMESTAMP NOT NULL,
			total_score REAL NOT NULL,
			block_count INTEGER NOT NULL,
			history_json TEXT
		)`,
		`CREATE INDEX idx_bad_actors_promoted_at ON bad_actors(promoted_at)`,

		`INSERT INTO schema_version (version, description) VALUES (2, 'Add bad actors tables')`,
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("failed to execute %q: %w", stmt[:min(len(stmt), 60)], err)
		}
	}

	return tx.Commit()
}

// --- Bad Actor Operations ---

// BadActorInfo represents a bad actor entry.
type BadActorInfo struct {
	IP          string
	PromotedAt  time.Time
	TotalScore  float64
	BlockCount  int
	HistoryJSON string
}

// ScoreInfo represents an IP's current score.
type ScoreInfo struct {
	Score         float64
	BlockCount    int
	LastBlockTime time.Time
}

// IncrementScore adds weight to an IP's score and returns the new score.
func IncrementScore(db *sql.DB, ip string, weight float64, timestamp time.Time) (float64, int, error) {
	var score float64
	var blockCount int
	err := db.QueryRow(`INSERT INTO ip_scores (ip, score, block_count, last_block_time)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(ip) DO UPDATE SET
			score = ip_scores.score + excluded.score,
			block_count = ip_scores.block_count + 1,
			last_block_time = excluded.last_block_time
		RETURNING score, block_count`,
		ip, weight, timestamp).Scan(&score, &blockCount)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to increment score: %w", err)
	}
	return score, blockCount, nil
}

// GetScore returns the score info for an IP, or nil if not found.
func GetScore(db *sql.DB, ip string) (*ScoreInfo, error) {
	var s ScoreInfo
	err := db.QueryRow("SELECT score, block_count, last_block_time FROM ip_scores WHERE ip = ?", ip).
		Scan(&s.Score, &s.BlockCount, &s.LastBlockTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get score: %w", err)
	}
	return &s, nil
}

// PromoteToBadActor inserts an IP into the bad_actors table with history from recent events.
func PromoteToBadActor(db *sql.DB, ip string, score float64, blockCount int, timestamp time.Time) error {
	// Build history from recent events
	rows, err := db.Query(`
		SELECT e.timestamp, r.reason
		FROM events e LEFT JOIN reasons r ON r.id = e.reason_id
		WHERE e.ip = ? AND e.event_type = 'block'
		ORDER BY e.timestamp DESC LIMIT 50`, ip)
	if err != nil {
		return fmt.Errorf("failed to query event history: %w", err)
	}
	defer rows.Close()

	type histEntry struct {
		Timestamp string `json:"ts"`
		Reason    string `json:"r"`
	}
	var history []histEntry
	for rows.Next() {
		var ts time.Time
		var reason sql.NullString
		if err := rows.Scan(&ts, &reason); err != nil {
			continue
		}
		history = append(history, histEntry{Timestamp: ts.Format(time.RFC3339), Reason: nullStringValue(reason)})
	}

	historyJSON, _ := json.Marshal(history)

	_, err = db.Exec(`INSERT OR REPLACE INTO bad_actors (ip, promoted_at, total_score, block_count, history_json)
		VALUES (?, ?, ?, ?, ?)`, ip, timestamp, score, blockCount, string(historyJSON))
	if err != nil {
		return fmt.Errorf("failed to promote to bad actor: %w", err)
	}
	return nil
}

// IsBadActor returns true if the IP is in the bad_actors table.
func IsBadActor(db *sql.DB, ip string) (bool, error) {
	var count int
	err := db.QueryRow("SELECT 1 FROM bad_actors WHERE ip = ?", ip).Scan(&count)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check bad actor: %w", err)
	}
	return true, nil
}

// GetBadActor returns bad actor info for an IP, or nil if not found.
func GetBadActor(db *sql.DB, ip string) (*BadActorInfo, error) {
	var ba BadActorInfo
	var histJSON sql.NullString
	err := db.QueryRow("SELECT ip, promoted_at, total_score, block_count, history_json FROM bad_actors WHERE ip = ?", ip).
		Scan(&ba.IP, &ba.PromotedAt, &ba.TotalScore, &ba.BlockCount, &histJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get bad actor: %w", err)
	}
	ba.HistoryJSON = nullStringValue(histJSON)
	return &ba, nil
}

// GetAllBadActors returns all bad actors.
func GetAllBadActors(db *sql.DB) ([]BadActorInfo, error) {
	rows, err := db.Query("SELECT ip, promoted_at, total_score, block_count, history_json FROM bad_actors ORDER BY promoted_at DESC")
	if err != nil {
		return nil, fmt.Errorf("failed to query bad actors: %w", err)
	}
	defer rows.Close()

	var result []BadActorInfo
	for rows.Next() {
		var ba BadActorInfo
		var histJSON sql.NullString
		if err := rows.Scan(&ba.IP, &ba.PromotedAt, &ba.TotalScore, &ba.BlockCount, &histJSON); err != nil {
			return nil, fmt.Errorf("failed to scan bad actor: %w", err)
		}
		ba.HistoryJSON = nullStringValue(histJSON)
		result = append(result, ba)
	}
	return result, rows.Err()
}

// RemoveBadActor removes an IP from bad_actors and resets its score.
func RemoveBadActor(db *sql.DB, ip string) error {
	_, err := db.Exec("DELETE FROM bad_actors WHERE ip = ?", ip)
	if err != nil {
		return fmt.Errorf("failed to remove bad actor: %w", err)
	}
	_, err = db.Exec("DELETE FROM ip_scores WHERE ip = ?", ip)
	if err != nil {
		return fmt.Errorf("failed to reset score: %w", err)
	}
	return nil
}

// CleanupLowScores removes old low-score entries from ip_scores.
func CleanupLowScores(db *sql.DB, maxAge time.Duration, minScore float64) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	res, err := db.Exec("DELETE FROM ip_scores WHERE score < ? AND last_block_time < ?", minScore, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup low scores: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
