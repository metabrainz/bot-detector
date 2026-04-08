package persistence

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ShouldMigrate returns true if legacy files exist and the database is empty.
func ShouldMigrate(stateDir string) bool {
	snapshotPath := filepath.Join(stateDir, "state.snapshot")
	journalPath := filepath.Join(stateDir, "events.log")

	_, snapErr := os.Stat(snapshotPath)
	_, journalErr := os.Stat(journalPath)

	return snapErr == nil || journalErr == nil
}

// MigrateFromLegacy imports state.snapshot and events.log into SQLite,
// then renames the legacy files to .migrated.
func MigrateFromLegacy(db *sql.DB, stateDir string) error {
	// Check if DB already has data
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM ips").Scan(&count); err != nil {
		return fmt.Errorf("failed to check existing data: %w", err)
	}
	if count > 0 {
		return nil // DB already populated, skip migration
	}

	snapshotPath := filepath.Join(stateDir, "state.snapshot")
	journalPath := filepath.Join(stateDir, "events.log")

	// Load snapshot
	var snapshotTimestamp time.Time
	if _, err := os.Stat(snapshotPath); err == nil {
		snapshot, err := LoadSnapshot(snapshotPath)
		if err != nil {
			return fmt.Errorf("failed to load snapshot: %w", err)
		}
		snapshotTimestamp = snapshot.Timestamp

		if err := importSnapshot(db, snapshot); err != nil {
			return fmt.Errorf("failed to import snapshot: %w", err)
		}
	}

	// Replay journal (only entries after snapshot timestamp)
	if _, err := os.Stat(journalPath); err == nil {
		if err := importJournal(db, journalPath, snapshotTimestamp); err != nil {
			return fmt.Errorf("failed to import journal: %w", err)
		}
	}

	// Rename legacy files to .migrated
	for _, path := range []string{snapshotPath, journalPath} {
		if _, err := os.Stat(path); err == nil {
			if err := os.Rename(path, path+".migrated"); err != nil {
				return fmt.Errorf("failed to rename %s: %w", path, err)
			}
		}
	}

	return nil
}

func importSnapshot(db *sql.DB, snapshot *Snapshot) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for ip, state := range snapshot.IPStates {
		reasonID, err := getOrCreateReasonIDTx(tx, state.Reason)
		if err != nil {
			return err
		}

		modifiedAt := state.ModifiedAt
		if modifiedAt.IsZero() {
			modifiedAt = snapshot.Timestamp
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO ips (ip, state, expire_time, reason_id, modified_at, first_blocked_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			ip, state.State.String(), state.ExpireTime, reasonID, modifiedAt, state.FirstBlockedAt)
		if err != nil {
			return fmt.Errorf("failed to insert IP %s: %w", ip, err)
		}
	}

	return tx.Commit()
}

func importJournal(db *sql.DB, journalPath string, after time.Time) error {
	f, err := os.Open(journalPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry JournalEntryV1
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // Skip unparseable lines
		}

		if !after.IsZero() && !entry.Timestamp.After(after) {
			continue // Skip entries already covered by snapshot
		}

		reasonID, err := getOrCreateReasonIDTx(tx, entry.Event.Reason)
		if err != nil {
			return err
		}

		// Insert event
		var durNanos *int64
		if entry.Event.Duration > 0 {
			d := int64(entry.Event.Duration)
			durNanos = &d
		}
		_, err = tx.Exec(`INSERT OR IGNORE INTO events (timestamp, event_type, ip, reason_id, duration, node_name)
			VALUES (?, ?, ?, ?, ?, NULL)`,
			entry.Timestamp, string(entry.Event.Type), entry.Event.IP, reasonID, durNanos)
		if err != nil {
			return fmt.Errorf("failed to insert event: %w", err)
		}

		// Update IP state
		switch entry.Event.Type {
		case EventTypeBlock:
			expireTime := entry.Timestamp.Add(entry.Event.Duration)
			_, err = tx.Exec(`INSERT INTO ips (ip, state, expire_time, reason_id, modified_at, first_blocked_at)
				VALUES (?, 'blocked', ?, ?, ?, ?)
				ON CONFLICT(ip) DO UPDATE SET
					state = 'blocked',
					expire_time = excluded.expire_time,
					reason_id = excluded.reason_id,
					modified_at = excluded.modified_at,
					first_blocked_at = CASE
						WHEN ips.first_blocked_at IS NOT NULL AND ips.first_blocked_at != '' AND ips.first_blocked_at < excluded.first_blocked_at
						THEN ips.first_blocked_at
						ELSE excluded.first_blocked_at
					END`,
				entry.Event.IP, expireTime, reasonID, entry.Timestamp, entry.Timestamp)
		case EventTypeUnblock:
			_, err = tx.Exec(`INSERT INTO ips (ip, state, expire_time, reason_id, modified_at)
				VALUES (?, 'unblocked', ?, ?, ?)
				ON CONFLICT(ip) DO UPDATE SET
					state = 'unblocked',
					expire_time = excluded.expire_time,
					reason_id = excluded.reason_id,
					modified_at = excluded.modified_at`,
				entry.Event.IP, entry.Timestamp, reasonID, entry.Timestamp)
		}
		if err != nil {
			return fmt.Errorf("failed to update IP state: %w", err)
		}
	}

	return tx.Commit()
}

func getOrCreateReasonIDTx(tx *sql.Tx, reason string) (int64, error) {
	id := reasonHash(reason)
	_, err := tx.Exec("INSERT OR IGNORE INTO reasons (id, reason) VALUES (?, ?)", id, reason)
	if err != nil {
		return 0, fmt.Errorf("failed to insert reason: %w", err)
	}
	return id, nil
}
