package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	mux := newMux(NewSeedctl(upstream.URL, upstream.Client()))
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

	mux := newMux(NewSeedctl(deadURL, http.DefaultClient))
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

	mux := newMux(NewSeedctl(upstream.URL, upstream.Client()))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body healthzResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.False(t, body.OK)
	assert.False(t, body.SeedctlOK)
}
