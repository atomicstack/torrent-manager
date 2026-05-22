package itemsdb_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/itemsdb"
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
