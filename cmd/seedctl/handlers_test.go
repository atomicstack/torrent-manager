package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/rtorrent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubClient implements the subset of rtorrent.Client used by handlers.
type stubClient struct {
	ping   func() (string, error)
	load   func([]byte, string, string) (string, error)
	status func(string) (api.TorrentStatus, error)
	list   func() ([]rtorrent.TorrentMetadata, error)
}

func (s *stubClient) Ping() (string, error)                             { return s.ping() }
func (s *stubClient) LoadTorrent(b []byte, h, d string) (string, error) { return s.load(b, h, d) }
func (s *stubClient) GetStatus(h string) (api.TorrentStatus, error)     { return s.status(h) }
func (s *stubClient) ListTorrents() ([]rtorrent.TorrentMetadata, error) {
	if s.list == nil {
		return nil, nil
	}
	return s.list()
}

func TestHealthz_OK(t *testing.T) {
	h := newHandler(&stubClient{ping: func() (string, error) { return "0.9.8", nil }}, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"ok":true`)
	assert.Contains(t, w.Body.String(), `"rtorrent_version":"0.9.8"`)
}

func TestHealthz_RTorrentDown(t *testing.T) {
	h := newHandler(&stubClient{ping: func() (string, error) { return "", errors.New("connection refused") }}, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 503, w.Code)
}

func TestListTorrents_Empty(t *testing.T) {
	h := newHandler(&stubClient{list: func() ([]rtorrent.TorrentMetadata, error) {
		return []rtorrent.TorrentMetadata{}, nil
	}}, nil)
	req := httptest.NewRequest("GET", "/v1/torrents", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"torrents":[]`)
}

func TestListTorrents_Populated(t *testing.T) {
	h := newHandler(&stubClient{list: func() ([]rtorrent.TorrentMetadata, error) {
		return []rtorrent.TorrentMetadata{
			{Hash: "AAAA000000000000000000000000000000000000", Name: "Bobs.Burgers.S16E14.mkv"},
			{Hash: "BBBB000000000000000000000000000000000000", Name: "Some.Movie.2024.mkv"},
		}, nil
	}}, nil)
	req := httptest.NewRequest("GET", "/v1/torrents", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "AAAA000000000000000000000000000000000000")
	assert.Contains(t, body, "Bobs.Burgers.S16E14.mkv")
	assert.Contains(t, body, "BBBB000000000000000000000000000000000000")
}

func TestListTorrents_Error(t *testing.T) {
	h := newHandler(&stubClient{list: func() ([]rtorrent.TorrentMetadata, error) {
		return nil, errors.New("scgi down")
	}}, nil)
	req := httptest.NewRequest("GET", "/v1/torrents", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 500, w.Code)
}

func TestGetTorrent_NotFound(t *testing.T) {
	h := newHandler(&stubClient{status: func(string) (api.TorrentStatus, error) {
		return api.TorrentStatus{}, rtorrent.ErrNotFound
	}}, nil)
	req := httptest.NewRequest("GET", "/v1/torrents/AAAA000000000000000000000000000000000000", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 404, w.Code)
}

func TestGetTorrent_Found(t *testing.T) {
	h := newHandler(&stubClient{status: func(string) (api.TorrentStatus, error) {
		return api.TorrentStatus{InfoHash: "AAA", Name: "X", Complete: true}, nil
	}}, nil)
	req := httptest.NewRequest("GET", "/v1/torrents/AAA", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"X"`)
	assert.Contains(t, w.Body.String(), `"complete":true`)
}

func TestPostTorrent_Adds(t *testing.T) {
	called := false
	h := newHandler(&stubClient{
		load: func(b []byte, _ string, _ string) (string, error) {
			called = true
			assert.Equal(t, "fake-bytes", string(b))
			return "DEADBEEF" + strings.Repeat("0", 32), nil
		},
		status: func(string) (api.TorrentStatus, error) {
			return api.TorrentStatus{}, rtorrent.ErrNotFound
		},
	}, nil)
	req := httptest.NewRequest("POST", "/v1/torrents", strings.NewReader("fake-bytes"))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", "DEADBEEF"+strings.Repeat("0", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	assert.True(t, called)
	assert.Contains(t, w.Body.String(), `"added":true`)
}

func TestPostTorrent_Duplicate(t *testing.T) {
	h := newHandler(&stubClient{
		// status returns existing torrent → handler short-circuits without calling load
		status: func(string) (api.TorrentStatus, error) {
			return api.TorrentStatus{InfoHash: "DEADBEEF" + strings.Repeat("0", 32), Name: "x"}, nil
		},
		load: func([]byte, string, string) (string, error) {
			t.Fatalf("load should not be called for duplicates")
			return "", nil
		},
	}, nil)
	req := httptest.NewRequest("POST", "/v1/torrents", strings.NewReader("ignored"))
	req.Header.Set("X-Info-Hash", "DEADBEEF"+strings.Repeat("0", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"added":false`)
}

func TestPostTorrent_AddsWithSaveDirectory(t *testing.T) {
	var capturedDir string
	h := newHandler(&stubClient{
		load: func(b []byte, _ string, dir string) (string, error) {
			capturedDir = dir
			return "DEADBEEF" + strings.Repeat("0", 32), nil
		},
		status: func(string) (api.TorrentStatus, error) {
			return api.TorrentStatus{}, rtorrent.ErrNotFound
		},
	}, nil)
	req := httptest.NewRequest("POST", "/v1/torrents", strings.NewReader("fake-bytes"))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", "DEADBEEF"+strings.Repeat("0", 32))
	req.Header.Set("X-Save-Directory", "/home/seedbox/t/tv/foo/season1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	assert.Equal(t, "/home/seedbox/t/tv/foo/season1", capturedDir)
}
