package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/itemsdb"
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
	return newHandler(&stubClient{}, db), db
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
	got, err := db.List(context.Background(), itemsdb.ListQuery{IncludeEnqueued: true, Limit: 10})
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
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/items?q=guy&include_enqueued=1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var resp api.ItemsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	require.Len(t, resp.Items, 1)
	assert.Contains(t, resp.Items[0].Title, "Guy Mont")
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
