package itemsdb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordEnqueue(t *testing.T, db *itemsdb.DB, hash, saveDir string) {
	t.Helper()
	require.NoError(t, db.RecordEnqueue(context.Background(), itemsdb.Enqueue{
		InfoHash: hash, SaveDir: saveDir, Source: itemsdb.SourceAuto, EnqueuedAt: time.Now(),
	}))
}

func TestCompletions_Lifecycle(t *testing.T) {
	ctx := context.Background()
	db := tmpDB(t)
	// title comes from the items index when the hash is known there.
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{{
		FeedName: "f", GUID: "g1", Title: "Only.Connect.S20E01", InfoHash: "AAAA",
		FirstSeenAt: time.Now(), LastSeenAt: time.Now(),
	}}))
	recordEnqueue(t, db, "AAAA", "/t/tv/only.connect/season20")
	recordEnqueue(t, db, "BBBB", "/t/movies")
	recordEnqueue(t, db, "CCCC", "/t/tv/gone")

	pending, err := db.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"AAAA", "BBBB", "CCCC"}, pending)

	at := time.Unix(1_760_000_000, 0)
	c, err := db.MarkCompleted(ctx, "AAAA", "Only.Connect.S20E01.1080p", at)
	require.NoError(t, err)
	assert.Equal(t, api.Completion{
		InfoHash: "AAAA", Name: "Only.Connect.S20E01.1080p", Title: "Only.Connect.S20E01",
		SaveDir: "/t/tv/only.connect/season20", FinishedAt: at.Unix(),
	}, c)
	require.NoError(t, db.MarkGone(ctx, "CCCC", at))

	pending, err = db.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"BBBB"}, pending, "completed and gone rows aren't re-checked")

	unacked, err := db.UnnotifiedCompletions(ctx)
	require.NoError(t, err)
	assert.Equal(t, []api.Completion{c}, unacked, "gone rows are never announced")

	require.NoError(t, db.AckCompletions(ctx, []string{"AAAA"}, at))
	unacked, err = db.UnnotifiedCompletions(ctx)
	require.NoError(t, err)
	assert.Empty(t, unacked)
}

func TestCompletions_ReEnqueueResetsState(t *testing.T) {
	ctx := context.Background()
	db := tmpDB(t)
	recordEnqueue(t, db, "AAAA", "/t/a")
	_, err := db.MarkCompleted(ctx, "AAAA", "a", time.Now())
	require.NoError(t, err)
	require.NoError(t, db.AckCompletions(ctx, []string{"AAAA"}, time.Now()))

	recordEnqueue(t, db, "AAAA", "/t/b")
	pending, err := db.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"AAAA"}, pending)
}

// TestMigration_BackfillsNotifiedAt: enqueues rows that predate completion
// tracking are marked as already notified, so turning the feature on doesn't
// announce the whole back catalogue.
func TestMigration_BackfillsNotifiedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "items.db")
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(`
CREATE TABLE enqueues (
  info_hash TEXT PRIMARY KEY, feed_name TEXT, guid TEXT,
  title TEXT, save_dir TEXT, source TEXT NOT NULL, enqueued_at INTEGER NOT NULL
);
INSERT INTO enqueues (info_hash, save_dir, source, enqueued_at) VALUES ('OLD1', '/t/x', 'auto', 1);`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	db, err := itemsdb.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	pending, err := db.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending, "pre-existing enqueues must not be checked")

	recordEnqueue(t, db, "NEW1", "/t/y")
	pending, err = db.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"NEW1"}, pending)

	// a second open must not re-run the backfill over new rows.
	require.NoError(t, db.Close())
	db2, err := itemsdb.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })
	pending, err = db2.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"NEW1"}, pending)
}
