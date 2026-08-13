package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeedctl_Healthz_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/healthz", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: true, RTorrentVersion: "x"})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	ok, err := c.Healthz(context.Background())
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestSeedctl_Healthz_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	ok, err := c.Healthz(context.Background())
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "status 503")
}

func TestSeedctl_ListItems_PassesQueryParams(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/items", r.URL.Path)
		gotURL = r.URL.RawQuery
		// confirm each expected key/value made it through (order-independent).
		q := r.URL.Query()
		assert.Equal(t, "guy", q.Get("q"))
		assert.Equal(t, "TrackerA (TV)", q.Get("feed"))
		assert.Equal(t, "all", q.Get("enqueued_filter"))
		assert.Equal(t, "10", q.Get("limit"))
		assert.Equal(t, "20", q.Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
			{ID: 1, FeedName: "TrackerA (TV)", GUID: "g1", Title: "Guy Mont Spelling Bee"},
		}})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	out, err := c.ListItems(context.Background(), ItemsQuery{
		Q:              "guy",
		Feed:           "TrackerA (TV)",
		EnqueuedFilter: itemsdb.FilterAll,
		Limit:          10,
		Offset:         20,
	})
	require.NoError(t, err)
	require.Len(t, out.Items, 1)
	assert.Equal(t, "Guy Mont Spelling Bee", out.Items[0].Title)
	// sanity check on the encoded querystring — must url-encode the space in
	// the feed name as %20 or +.
	assert.True(t,
		strings.Contains(gotURL, "feed=TrackerA+%28TV%29") || strings.Contains(gotURL, "feed=TrackerA%20%28TV%29"),
		"unexpected encoded query: %q", gotURL)
}

func TestSeedctl_ListItems_EmptyQueryOmitsParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no flags set → no query string at all.
		assert.Empty(t, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: nil})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	_, err := c.ListItems(context.Background(), ItemsQuery{})
	require.NoError(t, err)
}

func TestSeedctl_GetTorrentStatus_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/torrents/AAAA000000000000000000000000000000000000", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.TorrentStatus{InfoHash: "AAAA000000000000000000000000000000000000", Name: "x", Complete: true})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	st, ok, err := c.GetTorrentStatus(context.Background(), "aaaa000000000000000000000000000000000000")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "x", st.Name)
	assert.True(t, st.Complete)
}

func TestSeedctl_GetTorrentStatus_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	st, ok, err := c.GetTorrentStatus(context.Background(), "BBBB000000000000000000000000000000000000")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, api.TorrentStatus{}, st)
}

func TestSeedctl_PostTorrent_SendsHeadersAndBody(t *testing.T) {
	const hash = "DEADBEEF00000000000000000000000000000000"
	const saveDir = "/home/seedbox/t/tv/Foo/Season 1"
	wantBody := []byte("d8:announce...e") // arbitrary opaque bytes

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/torrents", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-bittorrent", r.Header.Get("Content-Type"))
		assert.Equal(t, hash, r.Header.Get("X-Info-Hash"))
		assert.Equal(t, saveDir, r.Header.Get("X-Save-Directory"))
		// no Content-Encoding header — .torrent bodies are sent uncompressed.
		assert.Empty(t, r.Header.Get("Content-Encoding"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, wantBody, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AddTorrentResponse{InfoHash: hash, Added: true})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	out, err := c.PostTorrent(context.Background(), hash, wantBody, saveDir)
	require.NoError(t, err)
	assert.True(t, out.Added)
	assert.Equal(t, hash, out.InfoHash)
}

// TestSeedctl_PostTorrent_MarksSourceManual: every enqueue that leaves tmweb is
// user-driven (manual pick, ✨ classify, or import), so it must be tagged
// manual to distinguish it from tmgr's automatic rule matches.
func TestSeedctl_PostTorrent_MarksSourceManual(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "manual", r.Header.Get("X-Enqueue-Source"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AddTorrentResponse{InfoHash: "X", Added: true})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	_, err := c.PostTorrent(context.Background(), "X", []byte("body"), "/tv/show/season1")
	require.NoError(t, err)
}

func TestSeedctl_PostTorrent_OmitsSaveDirWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("X-Save-Directory"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AddTorrentResponse{InfoHash: "X", Added: true})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	_, err := c.PostTorrent(context.Background(), "X", []byte("body"), "")
	require.NoError(t, err)
}

func TestSeedctl_PostTorrent_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	_, err := c.PostTorrent(context.Background(), "X", []byte("body"), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 422")
}

// TestSeedctl_ListDirs_OK exercises the happy path: seedctl returns a sorted
// list of subdir names and ListDirs decodes them as-is.
func TestSeedctl_ListDirs_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/dirs", r.URL.Path)
		require.Equal(t, "tv", r.URL.Query().Get("root"))
		_ = json.NewEncoder(w).Encode(api.DirsListResponse{Dirs: []string{"a", "b"}})
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	out, err := c.ListDirs(context.Background(), "tv")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, out.Dirs)
}

// TestSeedctl_ListDirs_NotFoundIsEmpty confirms that 404 (unknown root) is
// surfaced as an empty list, not an error — the no-rule flow can fall
// through to the manual-edit form without a special-case error type.
func TestSeedctl_ListDirs_NotFoundIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	out, err := c.ListDirs(context.Background(), "movies")
	require.NoError(t, err)
	assert.Empty(t, out.Dirs)
}

// TestSeedctl_ListDirs_5xxIsError ensures unexpected statuses surface as an
// error (callers log + flag auto-pick failure rather than silently render
// an empty proposal).
func TestSeedctl_ListDirs_5xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewSeedctl(srv.URL, srv.Client())
	_, err := c.ListDirs(context.Background(), "tv")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}
