package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	"github.com/atomicstack/torrent-manager/internal/rtorrent"
)

// completion notifications: seedctl polls rtorrent for enqueued torrents that
// haven't finished yet, records each one that completes, and streams it to
// subscribers (tmnotify) over GET /v1/completions/stream until a subscriber
// acks it. the seedbox can't reach the lan, so the lan side holds this
// stream open and seedctl pushes down it.

const (
	// completionPollInterval is how often the detector asks rtorrent about
	// unfinished enqueues. only those rows are checked, so a tick is a few
	// local scgi calls.
	completionPollInterval = 10 * time.Second
	// hubBuffer is the per-subscriber backlog. a subscriber that falls this
	// far behind is dropped; it replays from the db when it reconnects.
	hubBuffer = 64
)

// streamKeepalive spaces the sse comment lines that stop idle connections
// being reaped by anything in between. a var so tests can shorten it.
var streamKeepalive = 30 * time.Second

type completionHub struct {
	mu   sync.Mutex
	subs map[chan api.Completion]struct{}
}

func newCompletionHub() *completionHub {
	return &completionHub{subs: map[chan api.Completion]struct{}{}}
}

// subscribe registers a listener. the returned channel closes when cancel is
// called or when the subscriber overflows its buffer.
func (h *completionHub) subscribe() (<-chan api.Completion, func()) {
	ch := make(chan api.Completion, hubBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.drop(ch) }
}

func (h *completionHub) drop(ch chan api.Completion) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *completionHub) broadcast(c api.Completion) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- c:
		default:
			slog.Warn("completion_subscriber_overflow", "hash", c.InfoHash)
			delete(h.subs, ch)
			close(ch)
		}
	}
}

type completionDetector struct {
	c     rtorrentClient
	items *itemsdb.DB
	hub   *completionHub
	now   func() time.Time
}

func (d *completionDetector) run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.checkOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkOnce asks rtorrent about every unfinished enqueue. completed ones are
// recorded and broadcast; hashes rtorrent no longer has are retired without
// an announcement; any other error leaves the row for the next tick.
func (d *completionDetector) checkOnce(ctx context.Context) {
	hashes, err := d.items.PendingCompletionChecks(ctx)
	if err != nil {
		slog.Error("completion_pending_query", "err", err)
		return
	}
	for _, h := range hashes {
		st, err := d.c.GetStatus(h)
		switch {
		case errors.Is(err, rtorrent.ErrNotFound):
			slog.Info("completion_hash_gone", "hash", h)
			if err := d.items.MarkGone(ctx, h, d.now()); err != nil {
				slog.Error("completion_mark_gone", "hash", h, "err", err)
			}
		case err != nil:
			slog.Warn("completion_status", "hash", h, "err", err)
		case st.Complete:
			c, err := d.items.MarkCompleted(ctx, h, st.Name, d.now())
			if err != nil {
				slog.Error("completion_mark_completed", "hash", h, "err", err)
				continue
			}
			slog.Info("completion_detected", "hash", h, "name", st.Name)
			d.hub.broadcast(c)
		}
	}
}

// completionStream serves GET /v1/completions/stream: every completed-but-
// unacked row first (catch-up after a disconnect), then live completions.
// it subscribes before reading the backlog, so a completion landing in
// between can arrive twice — subscribers must tolerate duplicates.
func (h *handler) completionStream(w http.ResponseWriter, r *http.Request) {
	if h.items == nil || h.hub == nil {
		http.Error(w, "completions unavailable", http.StatusServiceUnavailable)
		return
	}
	live, cancel := h.hub.subscribe()
	defer cancel()
	backlog, err := h.items.UnnotifiedCompletions(r.Context())
	if err != nil {
		slog.Error("completion_backlog", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	send := func(c api.Completion) error {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: completion\ndata: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}
	if _, err := fmt.Fprint(w, ": connected\n\n"); err == nil {
		err = rc.Flush()
	}
	if err != nil {
		slog.Error("completion_stream_flush", "err", err)
		return
	}
	for _, c := range backlog {
		if err := send(c); err != nil {
			return
		}
	}

	keepalive := time.NewTicker(streamKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case c, ok := <-live:
			if !ok {
				return // overflowed: the client reconnects and replays
			}
			if err := send(c); err != nil {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// completionAck serves POST /v1/completions/ack.
func (h *handler) completionAck(w http.ResponseWriter, r *http.Request) {
	if h.items == nil {
		http.Error(w, "completions unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req api.CompletionAckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	hashes := make([]string, 0, len(req.Hashes))
	for _, s := range req.Hashes {
		hashes = append(hashes, strings.ToUpper(s))
	}
	if err := h.items.AckCompletions(r.Context(), hashes, time.Now()); err != nil {
		slog.Error("completion_ack", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
