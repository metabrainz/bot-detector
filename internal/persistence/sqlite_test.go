package persistence

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB("", true) // in-memory
	require.NoError(t, err)
	t.Cleanup(func() { _ = CloseDB(db) })
	return db
}

func TestOpenDB_WALMode(t *testing.T) {
	db := openTestDB(t)

	var journalMode string
	err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	require.NoError(t, err)
	assert.Equal(t, "memory", journalMode) // :memory: reports "memory" not "wal"
}

func TestOpenDB_OnDisk_WALMode(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir, false)
	require.NoError(t, err)
	defer func() { _ = CloseDB(db) }()

	var journalMode string
	err = db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	require.NoError(t, err)
	assert.Equal(t, "wal", journalMode)
}

func TestApplyMigrations_Idempotent(t *testing.T) {
	db := openTestDB(t)

	// Run migrations again — should be a no-op
	err := ApplyMigrations(db)
	require.NoError(t, err)

	// Verify schema version
	var version int
	err = db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&version)
	require.NoError(t, err)
	assert.Equal(t, 2, version)
}

func TestApplyMigrations_TablesExist(t *testing.T) {
	db := openTestDB(t)

	tables := []string{"schema_version", "reasons", "ips", "events", "ip_scores", "bad_actors"}
	for _, table := range tables {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		require.NoError(t, err, "table %s should exist", table)
	}
}

func TestGetOrCreateReasonID_Deduplication(t *testing.T) {
	db := openTestDB(t)

	id1, err := GetOrCreateReasonID(db, "test-reason")
	require.NoError(t, err)

	id2, err := GetOrCreateReasonID(db, "test-reason")
	require.NoError(t, err)

	assert.Equal(t, id1, id2, "same reason should produce same ID")

	id3, err := GetOrCreateReasonID(db, "different-reason")
	require.NoError(t, err)
	assert.NotEqual(t, id1, id3, "different reasons should produce different IDs")
}

func TestUpsertIPState_Insert(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)
	expire := now.Add(1 * time.Hour)

	err := UpsertIPState(db, "1.2.3.4", BlockStateBlocked, expire, "chain1", now, now)
	require.NoError(t, err)

	state, err := GetIPState(db, "1.2.3.4")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, BlockStateBlocked, state.State)
	assert.Equal(t, expire.UTC(), state.ExpireTime.UTC())
	assert.Equal(t, "chain1", state.Reason)
	assert.Equal(t, now.UTC(), state.ModifiedAt.UTC())
	assert.Equal(t, now.UTC(), state.FirstBlockedAt.UTC())
}

func TestUpsertIPState_Update(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)
	firstBlocked := now.Add(-1 * time.Hour)

	// Initial insert
	err := UpsertIPState(db, "1.2.3.4", BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, firstBlocked)
	require.NoError(t, err)

	// Update with later block — should preserve FirstBlockedAt
	later := now.Add(30 * time.Minute)
	err = UpsertIPState(db, "1.2.3.4", BlockStateBlocked, later.Add(2*time.Hour), "chain2", later, later)
	require.NoError(t, err)

	state, err := GetIPState(db, "1.2.3.4")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, "chain2", state.Reason)
	assert.Equal(t, firstBlocked.UTC(), state.FirstBlockedAt.UTC(), "FirstBlockedAt should be preserved")
}

func TestGetIPState_NotFound(t *testing.T) {
	db := openTestDB(t)

	state, err := GetIPState(db, "9.9.9.9")
	require.NoError(t, err)
	assert.Nil(t, state)
}

func TestDeleteIPState(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	err := UpsertIPState(db, "1.2.3.4", BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, now)
	require.NoError(t, err)

	err = DeleteIPState(db, "1.2.3.4")
	require.NoError(t, err)

	state, err := GetIPState(db, "1.2.3.4")
	require.NoError(t, err)
	assert.Nil(t, state)
}

func TestDeleteIPState_NotFound(t *testing.T) {
	db := openTestDB(t)

	// Should not error on non-existent IP
	err := DeleteIPState(db, "9.9.9.9")
	require.NoError(t, err)
}

func TestGetAllIPStates(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, now))
	require.NoError(t, UpsertIPState(db, "2.2.2.2", BlockStateUnblocked, now, "good-actor", now, time.Time{}))
	require.NoError(t, UpsertIPState(db, "3.3.3.3", BlockStateBlocked, now.Add(2*time.Hour), "chain2", now, now))

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 3)

	assert.Equal(t, BlockStateBlocked, states["1.1.1.1"].State)
	assert.Equal(t, BlockStateUnblocked, states["2.2.2.2"].State)
	assert.Equal(t, BlockStateBlocked, states["3.3.3.3"].State)
	assert.Equal(t, "chain1", states["1.1.1.1"].Reason)
	assert.Equal(t, "good-actor", states["2.2.2.2"].Reason)
}

func TestGetBlockedIPs(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Active block
	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, now))
	// Expired block
	require.NoError(t, UpsertIPState(db, "2.2.2.2", BlockStateBlocked, now.Add(-1*time.Minute), "chain2", now, now))
	// Unblocked
	require.NoError(t, UpsertIPState(db, "3.3.3.3", BlockStateUnblocked, now, "good-actor", now, time.Time{}))

	blocked, err := GetBlockedIPs(db, now)
	require.NoError(t, err)
	assert.Len(t, blocked, 1)
	assert.Contains(t, blocked, "1.1.1.1")
}

func TestInsertEvent(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	err := InsertEvent(db, now, EventTypeBlock, "1.2.3.4", "chain1", 1*time.Hour, "node1")
	require.NoError(t, err)

	// Verify event was inserted
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Verify fields
	var ip, eventType string
	var dur *int64
	var nodeName *string
	err = db.QueryRow("SELECT ip, event_type, duration, node_name FROM events WHERE ip = ?", "1.2.3.4").
		Scan(&ip, &eventType, &dur, &nodeName)
	require.NoError(t, err)
	assert.Equal(t, "1.2.3.4", ip)
	assert.Equal(t, "block", eventType)
	require.NotNil(t, dur)
	assert.Equal(t, int64(1*time.Hour), *dur)
	require.NotNil(t, nodeName)
	assert.Equal(t, "node1", *nodeName)
}

func TestInsertEvent_Duplicate(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	err := InsertEvent(db, now, EventTypeBlock, "1.2.3.4", "chain1", 1*time.Hour, "node1")
	require.NoError(t, err)

	// Same event again — should not error (INSERT OR IGNORE)
	err = InsertEvent(db, now, EventTypeBlock, "1.2.3.4", "chain1", 1*time.Hour, "node1")
	require.NoError(t, err)

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "duplicate should be ignored")
}

func TestInsertEvent_EmptyNodeName(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	err := InsertEvent(db, now, EventTypeBlock, "1.2.3.4", "chain1", 1*time.Hour, "")
	require.NoError(t, err)

	var nodeName *string
	err = db.QueryRow("SELECT node_name FROM events WHERE ip = ?", "1.2.3.4").Scan(&nodeName)
	require.NoError(t, err)
	assert.Nil(t, nodeName)
}

func TestInsertEvent_Unblock(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	err := InsertEvent(db, now, EventTypeUnblock, "1.2.3.4", "good-actor", 0, "")
	require.NoError(t, err)

	var dur *int64
	err = db.QueryRow("SELECT duration FROM events WHERE ip = ?", "1.2.3.4").Scan(&dur)
	require.NoError(t, err)
	assert.Nil(t, dur, "unblock events should have nil duration")
}

func TestCleanupExpiredBlocks(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Expired block
	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateBlocked, now.Add(-1*time.Minute), "chain1", now, now))
	// Active block
	require.NoError(t, UpsertIPState(db, "2.2.2.2", BlockStateBlocked, now.Add(1*time.Hour), "chain2", now, now))
	// Unblocked (should not be touched)
	require.NoError(t, UpsertIPState(db, "3.3.3.3", BlockStateUnblocked, now, "good-actor", now, time.Time{}))

	deleted, err := CleanupExpiredBlocks(db, now)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 2)
	assert.Contains(t, states, "2.2.2.2")
	assert.Contains(t, states, "3.3.3.3")
}

func TestCleanupOldUnblocked(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)
	retention := 24 * time.Hour

	// Old unblocked (should be cleaned)
	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateUnblocked, now.Add(-48*time.Hour), "good-actor", now, time.Time{}))
	// Recent unblocked (should be kept)
	require.NoError(t, UpsertIPState(db, "2.2.2.2", BlockStateUnblocked, now.Add(-1*time.Hour), "good-actor", now, time.Time{}))
	// Blocked (should not be touched)
	require.NoError(t, UpsertIPState(db, "3.3.3.3", BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, now))

	deleted, err := CleanupOldUnblocked(db, now, retention)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 2)
	assert.Contains(t, states, "2.2.2.2")
	assert.Contains(t, states, "3.3.3.3")
}

func TestCleanupOldEvents(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)
	retention := 24 * time.Hour

	// Old event
	require.NoError(t, InsertEvent(db, now.Add(-48*time.Hour), EventTypeBlock, "1.1.1.1", "chain1", 1*time.Hour, ""))
	// Recent event
	require.NoError(t, InsertEvent(db, now.Add(-1*time.Hour), EventTypeBlock, "2.2.2.2", "chain2", 1*time.Hour, ""))

	deleted, err := CleanupOldEvents(db, retention)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestCleanupOrphanedReasons(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Create reasons via IP state and event
	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateBlocked, now.Add(1*time.Hour), "used-reason", now, now))
	require.NoError(t, InsertEvent(db, now, EventTypeBlock, "2.2.2.2", "event-reason", 1*time.Hour, ""))

	// Create an orphaned reason
	_, err := GetOrCreateReasonID(db, "orphaned-reason")
	require.NoError(t, err)

	var reasonCount int
	err = db.QueryRow("SELECT COUNT(*) FROM reasons").Scan(&reasonCount)
	require.NoError(t, err)
	assert.Equal(t, 3, reasonCount)

	deleted, err := CleanupOrphanedReasons(db)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	err = db.QueryRow("SELECT COUNT(*) FROM reasons").Scan(&reasonCount)
	require.NoError(t, err)
	assert.Equal(t, 2, reasonCount)
}

func TestCloseDB_Nil(t *testing.T) {
	err := CloseDB(nil)
	assert.NoError(t, err)
}

func TestCloseDB_OnDisk(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir, false)
	require.NoError(t, err)

	// Insert some data
	now := time.Now().Truncate(time.Second)
	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateBlocked, now.Add(1*time.Hour), "chain1", now, now))

	// Close with checkpoint
	err = CloseDB(db)
	require.NoError(t, err)

	// Reopen and verify data survived
	db2, err := OpenDB(dir, false)
	require.NoError(t, err)
	defer func() { _ = CloseDB(db2) }()

	state, err := GetIPState(db2, "1.1.1.1")
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, BlockStateBlocked, state.State)
}

func TestReasonHash_Deterministic(t *testing.T) {
	h1 := reasonHash("test-reason")
	h2 := reasonHash("test-reason")
	assert.Equal(t, h1, h2)

	h3 := reasonHash("different")
	assert.NotEqual(t, h1, h3)
}

func TestIncrementScore(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	score, count, err := IncrementScore(db, "1.1.1.1", 1.0, now)
	require.NoError(t, err)
	assert.Equal(t, 1.0, score)
	assert.Equal(t, 1, count)

	score, count, err = IncrementScore(db, "1.1.1.1", 0.5, now.Add(1*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1.5, score)
	assert.Equal(t, 2, count)
}

func TestGetScore(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Not found
	s, err := GetScore(db, "9.9.9.9")
	require.NoError(t, err)
	assert.Nil(t, s)

	// After increment
	_, _, _ = IncrementScore(db, "1.1.1.1", 0.7, now)
	s, err = GetScore(db, "1.1.1.1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, 0.7, s.Score)
	assert.Equal(t, 1, s.BlockCount)
}

func TestPromoteToBadActor(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Insert some events for history
	require.NoError(t, InsertEvent(db, now.Add(-2*time.Hour), EventTypeBlock, "1.1.1.1", "chain1", 1*time.Hour, ""))
	require.NoError(t, InsertEvent(db, now.Add(-1*time.Hour), EventTypeBlock, "1.1.1.1", "chain2", 1*time.Hour, ""))

	err := PromoteToBadActor(db, "1.1.1.1", 5.0, 5, now)
	require.NoError(t, err)

	ba, err := GetBadActor(db, "1.1.1.1")
	require.NoError(t, err)
	require.NotNil(t, ba)
	assert.Equal(t, "1.1.1.1", ba.IP)
	assert.Equal(t, 5.0, ba.TotalScore)
	assert.Equal(t, 5, ba.BlockCount)
	assert.NotEmpty(t, ba.HistoryJSON)
}

func TestIsBadActor(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	is, err := IsBadActor(db, "1.1.1.1")
	require.NoError(t, err)
	assert.False(t, is)

	require.NoError(t, PromoteToBadActor(db, "1.1.1.1", 5.0, 5, now))

	is, err = IsBadActor(db, "1.1.1.1")
	require.NoError(t, err)
	assert.True(t, is)
}

func TestGetAllBadActors(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	require.NoError(t, PromoteToBadActor(db, "1.1.1.1", 5.0, 5, now))
	require.NoError(t, PromoteToBadActor(db, "2.2.2.2", 7.0, 10, now.Add(1*time.Second)))

	all, err := GetAllBadActors(db)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	// Ordered by promoted_at DESC
	assert.Equal(t, "2.2.2.2", all[0].IP)
	assert.Equal(t, "1.1.1.1", all[1].IP)
}

func TestRemoveBadActor(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	_, _, _ = IncrementScore(db, "1.1.1.1", 5.0, now)
	require.NoError(t, PromoteToBadActor(db, "1.1.1.1", 5.0, 5, now))

	require.NoError(t, RemoveBadActor(db, "1.1.1.1"))

	is, _ := IsBadActor(db, "1.1.1.1")
	assert.False(t, is)

	s, _ := GetScore(db, "1.1.1.1")
	assert.Nil(t, s)
}

func TestCleanupLowScores(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Old low score (should be cleaned)
	_, _, _ = IncrementScore(db, "1.1.1.1", 0.5, now.Add(-60*24*time.Hour))
	// Recent low score (should be kept)
	_, _, _ = IncrementScore(db, "2.2.2.2", 0.5, now.Add(-1*time.Hour))
	// Old high score (should be kept)
	_, _, _ = IncrementScore(db, "3.3.3.3", 3.0, now.Add(-60*24*time.Hour))

	deleted, err := CleanupLowScores(db, 30*24*time.Hour, 2.0)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	s, _ := GetScore(db, "1.1.1.1")
	assert.Nil(t, s, "old low score should be cleaned")

	s, _ = GetScore(db, "2.2.2.2")
	assert.NotNil(t, s, "recent low score should be kept")

	s, _ = GetScore(db, "3.3.3.3")
	assert.NotNil(t, s, "old high score should be kept")
}
