package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func completionsDB(t *testing.T, hashes ...string) *itemsdb.DB {
	t.Helper()
	db, err := itemsdb.Open(filepath.Join(t.TempDir(), "items.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, h := range hashes {
		require.NoError(t, db.RecordEnqueue(context.Background(), itemsdb.Enqueue{
			InfoHash: h, SaveDir: "/t/tv/" + strings.ToLower(h), Source: itemsdb.SourceAuto, EnqueuedAt: time.Now(),
		}))
	}
	return db
}

// statusStub answers GetStatus from a fixed table; missing hashes are
// ErrNotFound, "ERR" is a transient scgi failure.
func statusStub(table map[string]api.TorrentStatus) *stubClient {
	return &stubClient{status: func(h string) (api.TorrentStatus, error) {
		if h == "ERR" {
			return api.TorrentStatus{}, errors.New("scgi: connection refused")
		}
		st, ok := table[h]
		if !ok {
			return api.TorrentStatus{}, rtorrent.ErrNotFound
		}
		return st, nil
	}}
}

func TestDetector_CheckOnce(t *testing.T) {
	ctx := context.Background()
	db := completionsDB(t, "DONE", "BUSY", "GONE", "ERR")
	hub := newCompletionHub()
	sub, cancel := hub.subscribe()
	defer cancel()
	d := &completionDetector{
		c: statusStub(map[string]api.TorrentStatus{
			"DONE": {InfoHash: "DONE", Name: "done.mkv", Complete: true},
			"BUSY": {InfoHash: "BUSY", Name: "busy.mkv", Complete: false},
		}),
		items: db, hub: hub,
		now: func() time.Time { return time.Unix(1_760_000_000, 0) },
	}
	d.checkOnce(ctx)

	select {
	case c := <-sub:
		assert.Equal(t, "DONE", c.InfoHash)
		assert.Equal(t, "done.mkv", c.Name)
		assert.Equal(t, int64(1_760_000_000), c.FinishedAt)
	default:
		t.Fatal("completion wasn't broadcast")
	}
	pending, err := db.PendingCompletionChecks(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"BUSY", "ERR"}, pending, "done + gone retired; busy + transient error retried")
	unacked, err := db.UnnotifiedCompletions(ctx)
	require.NoError(t, err)
	require.Len(t, unacked, 1)
	assert.Equal(t, "DONE", unacked[0].InfoHash)
}

// readEvent reads one sse event (skipping keepalive comments).
func readEvent(t *testing.T, r *bufio.Reader) (event string, data string) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if event != "" || data != "" {
				return event, data
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// TestCompletionStream_ReplayThenLive runs through the production middleware
// stack (logging + gzip) on a real server: events must arrive while the
// stream is still open, so every wrapper has to pass flushes through.
func TestCompletionStream_ReplayThenLive(t *testing.T) {
	ctx := context.Background()
	db := completionsDB(t, "OLD1", "NEW1")
	_, err := db.MarkCompleted(ctx, "OLD1", "old.mkv", time.Unix(100, 0))
	require.NoError(t, err)

	hub := newCompletionHub()
	srv := httptest.NewServer(withLogging(withGzip(newHandlerWithHub(&stubClient{}, db, nil, nil, hub))))
	t.Cleanup(srv.Close)

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", srv.URL+"/v1/completions/stream", nil)
	require.NoError(t, err)
	// the go client asks for gzip by default; exercise that path explicitly.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Transport{DisableCompression: false}).RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Content-Encoding"), "sse must not be gzipped")
	r := bufio.NewReader(resp.Body)

	ev, data := readEvent(t, r)
	assert.Equal(t, "completion", ev)
	var c api.Completion
	require.NoError(t, json.Unmarshal([]byte(data), &c))
	assert.Equal(t, "OLD1", c.InfoHash)

	d := &completionDetector{
		c:     statusStub(map[string]api.TorrentStatus{"NEW1": {InfoHash: "NEW1", Name: "new.mkv", Complete: true}}),
		items: db, hub: hub, now: time.Now,
	}
	d.checkOnce(ctx)
	ev, data = readEvent(t, r)
	assert.Equal(t, "completion", ev)
	require.NoError(t, json.Unmarshal([]byte(data), &c))
	assert.Equal(t, "NEW1", c.InfoHash)
	assert.Equal(t, "new.mkv", c.Name)
}

func TestCompletionAck(t *testing.T) {
	ctx := context.Background()
	db := completionsDB(t, "AAAA", "BBBB")
	for _, h := range []string{"AAAA", "BBBB"} {
		_, err := db.MarkCompleted(ctx, h, h, time.Now())
		require.NoError(t, err)
	}
	h := newHandler(&stubClient{}, db, nil, nil)
	body, _ := json.Marshal(api.CompletionAckRequest{Hashes: []string{"aaaa"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/completions/ack", bytes.NewReader(body)))
	require.Equal(t, http.StatusNoContent, rr.Code)

	unacked, err := db.UnnotifiedCompletions(ctx)
	require.NoError(t, err)
	require.Len(t, unacked, 1)
	assert.Equal(t, "BBBB", unacked[0].InfoHash, "lowercase hash in the ack still matches")
}

// a subscriber that stops reading gets dropped rather than blocking the
// detector; it replays from the db when it reconnects.
func TestCompletionHub_DropsSlowSubscriber(t *testing.T) {
	hub := newCompletionHub()
	sub, cancel := hub.subscribe()
	defer cancel()
	for i := 0; i < hubBuffer+5; i++ {
		hub.broadcast(api.Completion{InfoHash: "X"})
	}
	n := 0
	for range sub {
		n++
	}
	assert.Equal(t, hubBuffer, n, "channel closed once the buffer overflowed")
}
