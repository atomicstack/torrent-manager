package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	"github.com/atomicstack/torrent-manager/internal/rtorrent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestItemsHandler wires the production handler against a fresh items db in
// a temp dir. The rtorrent stub is a no-op since these tests only exercise the
// /v1/items endpoints.
func newTestItemsHandler(t *testing.T) (http.Handler, *itemsdb.DB) {
	t.Helper()
	db, err := itemsdb.Open(filepath.Join(t.TempDir(), "items.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return newHandler(&stubClient{}, db, nil, nil), db
}

func TestUpsertItems_PersistsRows(t *testing.T) {
	h, db := newTestItemsHandler(t)
	body, _ := json.Marshal(api.ItemsUpsertRequest{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", GUID: "1", Title: "Foo S01E01", FirstSeenAt: 1, LastSeenAt: 1},
		{FeedName: "TrackerA (TV)", GUID: "2", Title: "Bar S02E02", FirstSeenAt: 1, LastSeenAt: 1},
	}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/items", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, rr.Code)
	got, err := db.List(context.Background(), itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestListItems_FTS5Search(t *testing.T) {
	h, db := newTestItemsHandler(t)
	now := time.Now()
	require.NoError(t, db.Upsert(context.Background(), []itemsdb.Item{
		{FeedName: "f", GUID: "1", Title: "Guy Mont Spelling Bee", FirstSeenAt: now, LastSeenAt: now},
		{FeedName: "f", GUID: "2", Title: "Hard Quiz", FirstSeenAt: now, LastSeenAt: now},
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/items?q=guy&enqueued_filter=all", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var resp api.ItemsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(t, resp.Items, 1)
	assert.Contains(t, resp.Items[0].Title, "Guy Mont")
}

func TestPostTorrent_RecordsEnqueue(t *testing.T) {
	hash := "DEADBEEF" + strings.Repeat("0", 32)
	saveDir := "/home/seedbox/t/tv/show/season1"

	db, err := itemsdb.Open(filepath.Join(t.TempDir(), "items.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// seed the item so the join in List can find the matching info_hash.
	now := time.Now()
	require.NoError(t, db.Upsert(context.Background(), []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "1", Title: "Show S01E01", InfoHash: hash, FirstSeenAt: now, LastSeenAt: now},
	}))

	h := newHandler(&stubClient{
		status: func(string) (api.TorrentStatus, error) {
			return api.TorrentStatus{}, rtorrent.ErrNotFound
		},
		load: func(_ []byte, gotHash, _ string) (string, error) {
			return gotHash, nil
		},
	}, db, nil, nil)

	req := httptest.NewRequest("POST", "/v1/torrents", strings.NewReader("fake-bytes"))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", hash)
	req.Header.Set("X-Save-Directory", saveDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	rows, err := db.List(context.Background(), itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Enqueued)
	assert.Equal(t, itemsdb.SourceAuto, rows[0].EnqueueSource)
}

func TestSetItemMeta_PersistsAndAppearsInList(t *testing.T) {
	h, db := newTestItemsHandler(t)
	now := time.Now()
	require.NoError(t, db.Upsert(context.Background(), []itemsdb.Item{
		{FeedName: "f", GUID: "g1", Title: "x", FirstSeenAt: now, LastSeenAt: now},
	}))
	body, _ := json.Marshal(api.ItemMetaRequest{
		FeedName: "f", GUID: "g1",
		TotalBytes: 1_610_612_736, FileCount: 12, DirCount: 3,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/items/meta", bytes.NewReader(body)))
	require.Equal(t, http.StatusNoContent, rr.Code)

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest("GET", "/v1/items?enqueued_filter=all", nil))
	require.Equal(t, http.StatusOK, rr2.Code)
	var resp api.ItemsListResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp))
	require.Len(t, resp.Items, 1)
	assert.Equal(t, int64(1_610_612_736), resp.Items[0].TotalBytes)
	assert.Equal(t, 12, resp.Items[0].FileCount)
	assert.Equal(t, 3, resp.Items[0].DirCount)
}

func TestSetItemMeta_BadJSON(t *testing.T) {
	h, _ := newTestItemsHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/items/meta", strings.NewReader("not json")))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestExportItems_ReturnsAll(t *testing.T) {
	h, db := newTestItemsHandler(t)
	now := time.Now()
	require.NoError(t, db.Upsert(context.Background(), []itemsdb.Item{
		{FeedName: "f", GUID: "1", Title: "X", FirstSeenAt: now, LastSeenAt: now},
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/items/export", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var resp api.ItemsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Len(t, resp.Items, 1)
}
