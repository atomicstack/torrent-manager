package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithLogging_EmitsOnePerRequest confirms the middleware writes a single
// structured "request" log line per inbound http request with the expected
// fields.
func TestWithLogging_EmitsOnePerRequest(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := withLogging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search?q=hello", strings.NewReader("")))
	require.Equal(t, http.StatusCreated, rr.Code)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "exactly one log line per request")
	var ev map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &ev))
	assert.Equal(t, "request", ev["msg"])
	assert.Equal(t, "GET", ev["method"])
	assert.Equal(t, "/api/search", ev["path"])
	assert.Equal(t, "q=hello", ev["query"])
	assert.Equal(t, float64(http.StatusCreated), ev["status"])
	assert.Equal(t, float64(2), ev["resp_bytes"], "wrote 2 bytes (\"ok\")")
}

// TestWithLogging_HealthzAtDebug demotes /healthz to DEBUG so the steady
// poll spam doesn't drown out the useful access logs.
func TestWithLogging_HealthzAtDebug(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := withLogging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, buf.String(), "/healthz must not log at INFO")
}
