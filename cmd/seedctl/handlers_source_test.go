package main

import (
	"context"
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

// postTorrentWithSource drives POST /v1/torrents with the given
// X-Enqueue-Source header value (skipped entirely when hdr is empty) and
// returns the source recorded in the enqueues table.
func postTorrentWithSource(t *testing.T, hdr string, setHdr bool) itemsdb.Source {
	t.Helper()
	hash := "DEADBEEF" + strings.Repeat("0", 32)

	db, err := itemsdb.Open(filepath.Join(t.TempDir(), "items.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	require.NoError(t, db.Upsert(context.Background(), []itemsdb.Item{
		{FeedName: "TrackerA (TV)", GUID: "1", Title: "Show S01E01", InfoHash: hash, FirstSeenAt: now, LastSeenAt: now},
	}))

	h := newHandler(&stubClient{
		status: func(string) (api.TorrentStatus, error) {
			return api.TorrentStatus{}, rtorrent.ErrNotFound
		},
		load: func(_ []byte, gotHash, _ string) (string, error) { return gotHash, nil },
	}, db, nil, nil)

	req := httptest.NewRequest("POST", "/v1/torrents", strings.NewReader("fake-bytes"))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", hash)
	if setHdr {
		req.Header.Set("X-Enqueue-Source", hdr)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	rows, _, err := db.List(context.Background(), itemsdb.ListQuery{EnqueuedFilter: itemsdb.FilterAll, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].Enqueued)
	return rows[0].EnqueueSource
}

// TestPostTorrent_SourceFromHeader: tmweb's manual enqueues must be
// distinguishable from tmgr's automatic ones in the enqueues table.
func TestPostTorrent_SourceFromHeader(t *testing.T) {
	assert.Equal(t, itemsdb.SourceManual, postTorrentWithSource(t, "manual", true))
	assert.Equal(t, itemsdb.SourceAuto, postTorrentWithSource(t, "auto", true))
}

// TestPostTorrent_SourceDefaultsToAuto: an older tmgr that doesn't send the
// header, or a client sending a value we don't recognise, must still record a
// valid source rather than writing junk into the column.
func TestPostTorrent_SourceDefaultsToAuto(t *testing.T) {
	assert.Equal(t, itemsdb.SourceAuto, postTorrentWithSource(t, "", false), "header absent")
	assert.Equal(t, itemsdb.SourceAuto, postTorrentWithSource(t, "", true), "header empty")
	assert.Equal(t, itemsdb.SourceAuto, postTorrentWithSource(t, "bogus", true), "header unrecognised")
	assert.Equal(t, itemsdb.SourceManual, postTorrentWithSource(t, "MANUAL", true), "header case-insensitive")
}
