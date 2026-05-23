package itemsdb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tmpDB(t *testing.T) *itemsdb.DB {
	t.Helper()
	db, err := itemsdb.Open(filepath.Join(t.TempDir(), "items.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUpsert_InsertThenUpdate_TouchesLastSeen(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now1 := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{{
		FeedName: "TrackerA (TV)", GUID: "g1",
		Title: "Foo S01E01 1080p", FirstSeenAt: now1, LastSeenAt: now1,
	}}))
	now2 := now1.Add(5 * time.Minute)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{{
		FeedName: "TrackerA (TV)", GUID: "g1",
		Title: "Foo S01E01 1080p", FirstSeenAt: now2, LastSeenAt: now2,
	}}))
	got, err := db.List(ctx, itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, now1.Unix(), got[0].FirstSeenAt.Unix(), "first_seen_at must not change on update")
	assert.Equal(t, now2.Unix(), got[0].LastSeenAt.Unix(), "last_seen_at must advance")
}

func TestList_FTS5SearchByPrefix(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "1", Title: "Guy Montgomery's Guy Mont-Spelling Bee S03E04", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "TrackerA (TV)", GUID: "2", Title: "Hard Quiz S19E03 1080p", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "TrackerB (TV)", GUID: "3", Title: "Only Connect S20E01", FirstSeenAt: now, LastSeenAt: now},
	}))

	got, err := db.List(ctx, itemsdb.ListQuery{Q: "guy mo", IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Title, "Guy Mont")

	got, err = db.List(ctx, itemsdb.ListQuery{Q: "quiz", IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Title, "Hard Quiz")

	// hyphenated tokens (e.g. "WEB-DL") must not be sent to fts5 with a
	// dangling prefix wildcard — that would parse as "WEB MINUS DL*" and
	// return wrong/empty results. The token should pass through verbatim.
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "4", Title: "Foo S01E01 1080p WEB-DL", FirstSeenAt: now, LastSeenAt: now},
	}))
	got, err = db.List(ctx, itemsdb.ListQuery{Q: "WEB-DL", IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Title, "WEB-DL")
}

func TestList_FeedFilter(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "1", Title: "Show A S01E01", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "TrackerB (TV)", GUID: "2", Title: "Show B S01E01", FirstSeenAt: now, LastSeenAt: now},
	}))
	got, err := db.List(ctx, itemsdb.ListQuery{Feed: "TrackerA (TV)", IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Show A S01E01", got[0].Title)
}

func TestExport_ReturnsAllRows(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "1", Title: "X", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "TrackerA (TV)", GUID: "2", Title: "Y", FirstSeenAt: now, LastSeenAt: now},
	}))
	rows, err := db.Export(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestList_ExcludeEnqueuedHidesRecorded(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "f", GUID: "1", Title: "kept",   InfoHash: "AAAA1111AAAA1111AAAA1111AAAA1111AAAA1111", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "f", GUID: "2", Title: "hidden", InfoHash: "BBBB2222BBBB2222BBBB2222BBBB2222BBBB2222", FirstSeenAt: now, LastSeenAt: now},
	}))
	require.NoError(t, db.RecordEnqueue(ctx, itemsdb.Enqueue{
		InfoHash: "BBBB2222BBBB2222BBBB2222BBBB2222BBBB2222",
		FeedName: "f", GUID: "2", Title: "hidden", SaveDir: "", Source: itemsdb.SourceAuto, EnqueuedAt: now,
	}))
	got, err := db.List(ctx, itemsdb.ListQuery{IncludeEnqueued: false, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "kept", got[0].Title)

	// the inverse must also be true: with IncludeEnqueued=true, both rows show.
	got, err = db.List(ctx, itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 2)

	// and the one with an enqueue must report Enqueued=true with source='auto'.
	var hiddenRow itemsdb.Item
	for _, r := range got {
		if r.Title == "hidden" {
			hiddenRow = r
		}
	}
	assert.True(t, hiddenRow.Enqueued)
	assert.Equal(t, itemsdb.SourceAuto, hiddenRow.EnqueueSource)
}

func TestRecordEnqueue_Idempotent(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	e := itemsdb.Enqueue{
		InfoHash:   "AABBCCDDEEFF11223344556677889900AABBCCDD",
		FeedName:   "f", GUID: "g",
		Title:      "x", SaveDir: "/t",
		Source:     itemsdb.SourceAuto, EnqueuedAt: now,
	}
	require.NoError(t, db.RecordEnqueue(ctx, e))
	require.NoError(t, db.RecordEnqueue(ctx, e)) // dup -> upsert, no error
}

// TestMigration_DropsEnclosureColumn seeds a db file with the pre-rename
// schema (items table with an enclosure column), opens it via the package's
// Open, and confirms the column is gone after migration. Guards the
// idempotent re-open path too.
func TestMigration_DropsEnclosureColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "items.db")

	// Hand-craft an old-shape items table (with enclosure column) using a
	// raw sqlite handle so we exercise the migration path on next Open.
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(`
CREATE TABLE items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  feed_name TEXT NOT NULL, guid TEXT NOT NULL,
  title TEXT NOT NULL, link TEXT, enclosure TEXT,
  info_hash TEXT, matched_rule TEXT,
  first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  UNIQUE(feed_name, guid)
);`)
	require.NoError(t, err)
	_, err = raw.Exec(`INSERT INTO items (feed_name, guid, title, link, enclosure, first_seen_at, last_seen_at)
VALUES ('f', 'g1', 't', 'L', 'E', 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Open via the package; migration should drop the column.
	db, err := itemsdb.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// PRAGMA inspection: the column must no longer exist.
	probe, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer probe.Close()
	var present int
	require.NoError(t, probe.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('items') WHERE name = 'enclosure'`,
	).Scan(&present))
	assert.Equal(t, 0, present, "enclosure column should be gone post-migration")

	// Existing row data still queryable (the data we don't keep is just gone).
	got, err := db.List(context.Background(), itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "L", got[0].Link)
}

// TestSetMeta_RoundTripsThroughList confirms SetMeta lands on the row and
// List returns the values; rows without a SetMeta call default to 0s.
func TestSetMeta_RoundTripsThroughList(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "f", GUID: "g1", Title: "with meta", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "f", GUID: "g2", Title: "no meta", FirstSeenAt: now, LastSeenAt: now},
	}))
	require.NoError(t, db.SetMeta(ctx, "f", "g1", 1_610_612_736, 12, 3))

	got, err := db.List(ctx, itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 2)
	byGUID := map[string]itemsdb.Item{}
	for _, it := range got {
		byGUID[it.GUID] = it
	}
	assert.Equal(t, int64(1_610_612_736), byGUID["g1"].TotalBytes)
	assert.Equal(t, 12, byGUID["g1"].FileCount)
	assert.Equal(t, 3, byGUID["g1"].DirCount)
	assert.Equal(t, int64(0), byGUID["g2"].TotalBytes, "rows without SetMeta read back as 0 via COALESCE")

	// Upsert must not clobber meta on subsequent ticks (RSS path doesn't
	// know meta — it leaves the columns alone).
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "f", GUID: "g1", Title: "with meta", FirstSeenAt: now, LastSeenAt: now.Add(time.Minute)},
	}))
	got, err = db.List(ctx, itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	for _, it := range got {
		if it.GUID == "g1" {
			assert.Equal(t, int64(1_610_612_736), it.TotalBytes, "Upsert must preserve total_bytes")
			assert.Equal(t, 12, it.FileCount, "Upsert must preserve file_count")
			assert.Equal(t, 3, it.DirCount, "Upsert must preserve dir_count")
		}
	}
}

// TestMigration_AddsMetaColumns seeds a pre-meta schema and confirms Open
// adds the new columns idempotently. Re-opening must not fail.
func TestMigration_AddsMetaColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "items.db")
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(`
CREATE TABLE items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  feed_name TEXT NOT NULL, guid TEXT NOT NULL,
  title TEXT NOT NULL, link TEXT,
  info_hash TEXT, matched_rule TEXT,
  first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  UNIQUE(feed_name, guid)
);`)
	require.NoError(t, err)
	_, err = raw.Exec(`INSERT INTO items (feed_name, guid, title, first_seen_at, last_seen_at)
VALUES ('f', 'g1', 't', 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	db, err := itemsdb.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, db.SetMeta(context.Background(), "f", "g1", 100, 2, 1))
	got, err := db.List(context.Background(), itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(100), got[0].TotalBytes)

	// Reopen — migration must be idempotent.
	require.NoError(t, db.Close())
	db2, err := itemsdb.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })
}

// TestList_OrderMirrorsRSSFeed pins down the new ordering contract: items
// from later ticks come first (newer first_seen_at), and within a single
// tick the insertion order (which mirrors the RSS XML order tmgr iterates)
// is preserved.
func TestList_OrderMirrorsRSSFeed(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()

	// tick 1: rss had [A, B, C]. A is newest-in-feed → first inserted.
	t1 := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "f", GUID: "a", Title: "A", FirstSeenAt: t1, LastSeenAt: t1},
		{FeedName: "f", GUID: "b", Title: "B", FirstSeenAt: t1, LastSeenAt: t1},
		{FeedName: "f", GUID: "c", Title: "C", FirstSeenAt: t1, LastSeenAt: t1},
	}))

	// tick 2: rss now has [D, A, B, C]. D is newer than A,B,C.
	t2 := t1.Add(5 * time.Minute)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "f", GUID: "d", Title: "D", FirstSeenAt: t2, LastSeenAt: t2},
		{FeedName: "f", GUID: "a", Title: "A", FirstSeenAt: t2, LastSeenAt: t2}, // upserts; first_seen_at unchanged
		{FeedName: "f", GUID: "b", Title: "B", FirstSeenAt: t2, LastSeenAt: t2},
		{FeedName: "f", GUID: "c", Title: "C", FirstSeenAt: t2, LastSeenAt: t2},
	}))

	got, err := db.List(ctx, itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
	require.NoError(t, err)
	titles := make([]string, 0, len(got))
	for _, r := range got {
		titles = append(titles, r.Title)
	}
	// Expected: D (only-in-tick-2, newest first_seen_at), then A B C ordered
	// by their insertion ids (matching rss xml order from tick 1).
	assert.Equal(t, []string{"D", "A", "B", "C"}, titles)
}
