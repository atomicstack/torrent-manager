package itemsdb_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
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
	got, _, err := db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, now1.Unix(), got[0].FirstSeenAt.Unix(), "first_seen_at must not change on update")
	assert.Equal(t, now2.Unix(), got[0].LastSeenAt.Unix(), "last_seen_at must advance")
}

// A torrent reaching the index via BOTH paths — manual import (guid =
// info_hash) and the rss poll (guid = the feed's <guid> url) — must
// collapse to a single row. Item identity is UNIQUE(feed_name, guid), but
// the two paths disagree on guid while agreeing on info_hash, so without a
// hash-level merge the same torrent shows up twice in the ui. Regression
// for the "duplicate house of the dragon" report.
func TestUpsert_SameHashDifferentGUID_Dedups(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	const hash = "E7403CD5F609251A7DEF8F26646F7D6DFEF29CF8"

	// manual import lands first: guid == info_hash.
	imp := time.Unix(1_783_300_184, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{{
		FeedName: "HPJ (TV)", GUID: hash,
		Title:    "House.of.the.Dragon.S03E03.DV.HDR.2160p.WEB.h265-ETHEL.mkv",
		Link:     "https://fakedoor.store/d.php?r=e7403cd5f609251a7def8f26646f7d6dfef29cf8",
		InfoHash: hash, FirstSeenAt: imp, LastSeenAt: imp,
	}}))

	// rss poll discovers the same torrent 29s later with the feed's url guid.
	rss := imp.Add(29 * time.Second)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{{
		FeedName: "HPJ (TV)", GUID: "https://fakedoor.store/d.php?r=e7403cd5f609251a7def8f26646f7d6dfef29cf8",
		Title:    "House.of.the.Dragon.S03E03.DV.HDR.2160p.WEB.h265-ETHEL.mkv",
		Link:     "https://fakedoor.store/d.php?r=e7403cd5f609251a7def8f26646f7d6dfef29cf8",
		InfoHash: hash, MatchedRule: "house of the dragon", FirstSeenAt: rss, LastSeenAt: rss,
	}}))

	got, _, err := db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1, "same (feed, info_hash) via two guids must be one row")
	assert.Equal(t, imp.Unix(), got[0].FirstSeenAt.Unix(), "first_seen_at kept from the earlier (manual-import) row")
	assert.Equal(t, rss.Unix(), got[0].LastSeenAt.Unix(), "last_seen_at advances to the rss sighting")
	assert.Equal(t, "house of the dragon", got[0].MatchedRule, "matched_rule from the rss row is merged in")
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

	got, _, err := db.List(ctx, itemsdb.ListQuery{Q: "guy mo", EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Title, "Guy Mont")

	got, _, err = db.List(ctx, itemsdb.ListQuery{Q: "quiz", EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Title, "Hard Quiz")

	// hyphenated tokens (e.g. "WEB-DL") must not be sent to fts5 with a
	// dangling prefix wildcard — that would parse as "WEB MINUS DL*" and
	// return wrong/empty results. The token should pass through verbatim.
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "4", Title: "Foo S01E01 1080p WEB-DL", FirstSeenAt: now, LastSeenAt: now},
	}))
	got, _, err = db.List(ctx, itemsdb.ListQuery{Q: "WEB-DL", EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
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
	got, _, err := db.List(ctx, itemsdb.ListQuery{Feed: "TrackerA (TV)", EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
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
	got, _, err := db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterUnenqueued, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "kept", got[0].Title)

	// the inverse must also be true: with EnqueuedFilter=FilterAll, both rows show.
	got, _, err = db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
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

	// FilterEnqueued is the third state — only items recorded as enqueued.
	got, _, err = db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterEnqueued, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "hidden", got[0].Title, "FilterEnqueued returns only the enqueued row")
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
	got, _, err := db.List(context.Background(), itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
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
	require.NoError(t, db.SetMeta(ctx, api.ItemMetaRequest{
		FeedName: "f", GUID: "g1",
		TotalBytes: 1_610_612_736, FileCount: 12, DirCount: 3,
	}))

	got, _, err := db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
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
	got, _, err = db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	for _, it := range got {
		if it.GUID == "g1" {
			assert.Equal(t, int64(1_610_612_736), it.TotalBytes, "Upsert must preserve total_bytes")
			assert.Equal(t, 12, it.FileCount, "Upsert must preserve file_count")
			assert.Equal(t, 3, it.DirCount, "Upsert must preserve dir_count")
		}
	}
}

// TestSetMeta_BackfillsInfoHashWhenMissing covers the TrackerB case: the rss
// feed didn't carry the hash so the Upsert left it null; the post-parse
// SetMeta must fill it in so the enqueued-badge join (items.info_hash =
// enqueues.info_hash) matches. Pre-filled rows must not be overwritten.
func TestSetMeta_BackfillsInfoHashWhenMissing(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, db.Upsert(ctx, []itemsdb.Item{
		{FeedName: "TrackerB (Movies)", GUID: "g1", Title: "no hash from rss", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "TrackerA (TV)", GUID: "g2", Title: "hash from rss", InfoHash: "AAAA1111AAAA1111AAAA1111AAAA1111AAAA1111", FirstSeenAt: now, LastSeenAt: now},
	}))
	require.NoError(t, db.SetMeta(ctx, api.ItemMetaRequest{
		FeedName: "TrackerB (Movies)", GUID: "g1",
		InfoHash:   "DEADBEEF00000000000000000000000000000000",
		TotalBytes: 100, FileCount: 1, DirCount: 0,
	}))
	require.NoError(t, db.SetMeta(ctx, api.ItemMetaRequest{
		FeedName: "TrackerA (TV)", GUID: "g2",
		InfoHash:   "BBBB2222BBBB2222BBBB2222BBBB2222BBBB2222",
		TotalBytes: 200, FileCount: 1, DirCount: 0,
	}))

	got, _, err := db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	byGUID := map[string]itemsdb.Item{}
	for _, it := range got {
		byGUID[it.GUID] = it
	}
	assert.Equal(t, "DEADBEEF00000000000000000000000000000000", byGUID["g1"].InfoHash, "missing hash backfilled from SetMeta")
	assert.Equal(t, "AAAA1111AAAA1111AAAA1111AAAA1111AAAA1111", byGUID["g2"].InfoHash, "existing hash preserved (SetMeta must not overwrite)")
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

	require.NoError(t, db.SetMeta(context.Background(), api.ItemMetaRequest{
		FeedName: "f", GUID: "g1",
		TotalBytes: 100, FileCount: 2, DirCount: 1,
	}))
	got, _, err := db.List(context.Background(), itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
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

	got, _, err := db.List(ctx, itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	titles := make([]string, 0, len(got))
	for _, r := range got {
		titles = append(titles, r.Title)
	}
	// Expected: D (only-in-tick-2, newest first_seen_at), then A B C ordered
	// by their insertion ids (matching rss xml order from tick 1).
	assert.Equal(t, []string{"D", "A", "B", "C"}, titles)
}

// Exact (feed, guid) and (feed, info_hash) lookups must find a row no matter
// how deep it sits in the feed's recency order. tmweb used to scan only the
// newest 1000 rows of a feed, so enqueueing an older item failed with
// "item not found".
func TestList_GUIDAndInfoHashFilters_FindOldRows(t *testing.T) {
	db := tmpDB(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	var batch []itemsdb.Item
	for i := range 1500 {
		ts := base.Add(time.Duration(i) * time.Minute)
		batch = append(batch, itemsdb.Item{
			FeedName: "HPJ (TV)", GUID: fmt.Sprintf("https://tracker.test/d.php?r=%040d", i),
			Title: fmt.Sprintf("Show S01E%04d", i), InfoHash: fmt.Sprintf("%040d", i),
			FirstSeenAt: ts, LastSeenAt: ts,
		})
	}
	require.NoError(t, db.Upsert(ctx, batch))

	oldest := batch[0]
	got, total, err := db.List(ctx, itemsdb.ListQuery{
		Feed: "HPJ (TV)", GUID: oldest.GUID, EnqueuedFilter: itemsdb.FilterAll, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, total)
	assert.Equal(t, oldest.Title, got[0].Title)

	got, _, err = db.List(ctx, itemsdb.ListQuery{
		Feed: "HPJ (TV)", InfoHash: oldest.InfoHash, EnqueuedFilter: itemsdb.FilterAll, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, oldest.GUID, got[0].GUID)

	got, _, err = db.List(ctx, itemsdb.ListQuery{
		Feed: "HPJ (TV)", GUID: "nope", EnqueuedFilter: itemsdb.FilterAll, Limit: 1,
	})
	require.NoError(t, err)
	assert.Empty(t, got)
}
