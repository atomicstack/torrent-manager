package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// muxFor builds a test mux against the given seedctl base url + http client.
// Centralised so we don't repeat the renderer + feeds plumbing in every
// test. Feeds defaults to nil — tests that exercise the dropdown override.
func muxFor(t *testing.T, base string, hc *http.Client, feeds []string) http.Handler {
	t.Helper()
	r, err := newRenderer()
	require.NoError(t, err)
	return newMux(NewSeedctl(base, hc), feeds, r)
}

// TestHealthz_SeedctlUp spins up an httptest server impersonating a healthy
// seedctl, points the Seedctl client at it, and asserts /healthz reports
// seedctl_ok: true.
func TestHealthz_SeedctlUp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/healthz", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: true, RTorrentVersion: "0.9.8"})
	}))
	t.Cleanup(upstream.Close)

	mux := muxFor(t, upstream.URL, upstream.Client(), nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body healthzResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.True(t, body.OK)
	assert.True(t, body.SeedctlOK)
}

// TestHealthz_SeedctlDown points the client at a non-existent server and
// asserts /healthz still returns 200 but reports seedctl_ok: false.
func TestHealthz_SeedctlDown(t *testing.T) {
	// reserve a port then close it: the next dial should be refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	mux := muxFor(t, deadURL, http.DefaultClient, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body healthzResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.False(t, body.OK)
	assert.False(t, body.SeedctlOK)
}

// TestHealthz_SeedctlReturnsNotHealthy covers the case where seedctl is
// reachable but reports ok=false (e.g. rtorrent is down). tmweb should
// surface seedctl_ok=false without erroring out.
func TestHealthz_SeedctlReturnsNotHealthy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: false})
	}))
	t.Cleanup(upstream.Close)

	mux := muxFor(t, upstream.URL, upstream.Client(), nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body healthzResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.False(t, body.OK)
	assert.False(t, body.SeedctlOK)
}

// TestIndex_ServesHTMLPage hits GET / and confirms the layout shell renders
// with the configured feed dropdown options. We stub seedctl so the page
// load doesn't depend on a live upstream — htmx will hit /api/search on its
// own once loaded in a real browser.
func TestIndex_ServesHTMLPage(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"ok":true,"rtorrent_version":"x"}`))
			return
		}
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{FeedName: "TrackerA (TV)", Title: "X", LastSeenAt: time.Now().Unix()},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stub.Close)

	mux := muxFor(t, stub.URL, stub.Client(), []string{"TrackerA (TV)"})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "<title>tmweb</title>")
	assert.Contains(t, body, `<option value="TrackerA (TV)">`)
	// content-type should be html so curl + browsers don't render-as-text
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/html")
}

// TestSearch_RendersTableRows hits /api/search and confirms we get back an
// html fragment (not json) containing the upstream item's title and the
// auto-enqueued badge.
func TestSearch_RendersTableRows(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
			{FeedName: "TrackerA (TV)", Title: "Search Hit", InfoHash: "DEAD", LastSeenAt: time.Now().Unix(), Enqueued: true, EnqueueSource: "auto"},
		}})
	}))
	t.Cleanup(stub.Close)

	mux := muxFor(t, stub.URL, stub.Client(), []string{"TrackerA (TV)"})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search?q=hit", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Search Hit")
	assert.Contains(t, body, "badge-auto")
}

// TestSearch_SeedctlErrorRendersErrorRow ensures that an unreachable
// seedctl yields a non-empty html fragment (an error row) so the table
// doesn't get stuck on "loading…".
func TestSearch_SeedctlErrorRendersErrorRow(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	mux := muxFor(t, deadURL, http.DefaultClient, []string{"TrackerA (TV)"})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "error:")
	assert.Contains(t, body, `class="empty"`)
}

// TestStatic_ServesCSS confirms the embedded css is reachable at
// /static/app.css — basic smoke test for the embed wiring.
func TestStatic_ServesCSS(t *testing.T) {
	mux := muxFor(t, "http://unused", http.DefaultClient, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), ":root")
}
