package persistence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldMigrate_NoFiles(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, ShouldMigrate(dir))
}

func TestShouldMigrate_SnapshotOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.snapshot"), []byte("{}"), 0644))
	assert.True(t, ShouldMigrate(dir))
}

func TestShouldMigrate_JournalOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "events.log"), []byte(""), 0644))
	assert.True(t, ShouldMigrate(dir))
}

func TestMigrateFromLegacy_SkipsIfDBHasData(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Pre-populate DB
	require.NoError(t, UpsertIPState(db, "1.1.1.1", BlockStateBlocked, now.Add(1*time.Hour), "existing", now, now))

	dir := t.TempDir()
	// Create a snapshot that would add different data
	writeTestSnapshot(t, dir, now, map[string]IPState{
		"2.2.2.2": {State: BlockStateBlocked, ExpireTime: now.Add(1 * time.Hour), Reason: "chain1"},
	})

	err := MigrateFromLegacy(db, dir)
	require.NoError(t, err)

	// Should still only have the pre-existing IP
	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 1)
	assert.Contains(t, states, "1.1.1.1")
}

func TestMigrateFromLegacy_SnapshotOnly(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	writeTestSnapshot(t, dir, now, map[string]IPState{
		"1.1.1.1": {State: BlockStateBlocked, ExpireTime: now.Add(1 * time.Hour), Reason: "chain1"},
		"2.2.2.2": {State: BlockStateUnblocked, ExpireTime: now, Reason: "good-actor"},
	})

	err := MigrateFromLegacy(db, dir)
	require.NoError(t, err)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 2)
	assert.Equal(t, BlockStateBlocked, states["1.1.1.1"].State)
	assert.Equal(t, "chain1", states["1.1.1.1"].Reason)
	assert.Equal(t, BlockStateUnblocked, states["2.2.2.2"].State)
	assert.Equal(t, "good-actor", states["2.2.2.2"].Reason)

	// Verify legacy file renamed
	assert.NoFileExists(t, filepath.Join(dir, "state.snapshot"))
	assert.FileExists(t, filepath.Join(dir, "state.snapshot.migrated"))
}

func TestMigrateFromLegacy_JournalOnly(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	writeTestJournal(t, dir, []JournalEntryV1{
		{Timestamp: now, Event: AuditEventDataV1{Type: EventTypeBlock, IP: "3.3.3.3", Duration: 1 * time.Hour, Reason: "chain-x"}},
		{Timestamp: now.Add(1 * time.Second), Event: AuditEventDataV1{Type: EventTypeUnblock, IP: "4.4.4.4", Reason: "good-actor"}},
	})

	err := MigrateFromLegacy(db, dir)
	require.NoError(t, err)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 2)
	assert.Equal(t, BlockStateBlocked, states["3.3.3.3"].State)
	assert.Equal(t, "chain-x", states["3.3.3.3"].Reason)
	assert.Equal(t, BlockStateUnblocked, states["4.4.4.4"].State)

	// Verify events were inserted
	var eventCount int
	err = db.QueryRow("SELECT COUNT(*) FROM events").Scan(&eventCount)
	require.NoError(t, err)
	assert.Equal(t, 2, eventCount)

	// Verify legacy file renamed
	assert.NoFileExists(t, filepath.Join(dir, "events.log"))
	assert.FileExists(t, filepath.Join(dir, "events.log.migrated"))
}

func TestMigrateFromLegacy_SnapshotAndJournal(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	// Snapshot has one blocked IP
	writeTestSnapshot(t, dir, now, map[string]IPState{
		"1.1.1.1": {State: BlockStateBlocked, ExpireTime: now.Add(1 * time.Hour), Reason: "chain1"},
	})

	// Journal has an event before snapshot (should be skipped) and one after
	writeTestJournal(t, dir, []JournalEntryV1{
		{Timestamp: now.Add(-1 * time.Second), Event: AuditEventDataV1{Type: EventTypeBlock, IP: "old.old.old.old", Duration: 1 * time.Hour, Reason: "old"}},
		{Timestamp: now.Add(1 * time.Second), Event: AuditEventDataV1{Type: EventTypeBlock, IP: "2.2.2.2", Duration: 2 * time.Hour, Reason: "chain2"}},
	})

	err := MigrateFromLegacy(db, dir)
	require.NoError(t, err)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Len(t, states, 2, "should have snapshot IP + journal IP, not the old one")
	assert.Contains(t, states, "1.1.1.1")
	assert.Contains(t, states, "2.2.2.2")
	assert.Equal(t, "chain2", states["2.2.2.2"].Reason)

	// Only the post-snapshot event should be in events table
	var eventCount int
	err = db.QueryRow("SELECT COUNT(*) FROM events").Scan(&eventCount)
	require.NoError(t, err)
	assert.Equal(t, 1, eventCount)
}

func TestMigrateFromLegacy_NoFiles(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	// Should be a no-op, no error
	err := MigrateFromLegacy(db, dir)
	require.NoError(t, err)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.Empty(t, states)
}

func TestMigrateFromLegacy_RealTestdata(t *testing.T) {
	db := openTestDB(t)

	// Copy testdata files to a temp dir so migration can rename them
	dir := t.TempDir()
	srcSnapshot := filepath.Join("..", "..", "testdata", "v1", "state.snapshot")
	srcJournal := filepath.Join("..", "..", "testdata", "v1", "events.log")

	snapshotData, err := os.ReadFile(srcSnapshot)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.snapshot"), snapshotData, 0644))

	journalData, err := os.ReadFile(srcJournal)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "events.log"), journalData, 0644))

	err = MigrateFromLegacy(db, dir)
	require.NoError(t, err)

	states, err := GetAllIPStates(db)
	require.NoError(t, err)
	assert.NotEmpty(t, states, "should have imported IPs from real testdata")

	// Verify files were renamed
	assert.FileExists(t, filepath.Join(dir, "state.snapshot.migrated"))
	assert.FileExists(t, filepath.Join(dir, "events.log.migrated"))
	assert.NoFileExists(t, filepath.Join(dir, "state.snapshot"))
	assert.NoFileExists(t, filepath.Join(dir, "events.log"))
}

// --- test helpers ---

func writeTestSnapshot(t *testing.T, dir string, ts time.Time, ipStates map[string]IPState) {
	t.Helper()
	snap := &Snapshot{
		Version:   CurrentVersion,
		Timestamp: ts,
		IPStates:  ipStates,
	}
	path := filepath.Join(dir, "state.snapshot")
	require.NoError(t, WriteSnapshot(path, snap))
}

func writeTestJournal(t *testing.T, dir string, entries []JournalEntryV1) {
	t.Helper()
	path := filepath.Join(dir, "events.log")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		require.NoError(t, err)
		_, err = f.Write(append(data, '\n'))
		require.NoError(t, err)
	}
}
