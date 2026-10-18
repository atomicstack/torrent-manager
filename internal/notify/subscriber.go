// Package notify turns seedctl's completion stream into mqtt messages: a
// Subscriber holds GET /v1/completions/stream open, hands each completion to
// a Publisher, and acks it once published. used by cmd/tmnotify.
package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// Publisher delivers one completion somewhere (mqtt in production).
type Publisher interface {
	Publish(ctx context.Context, c api.Completion) error
}

const (
	// idleTimeout drops a stream that has been silent this long. seedctl
	// sends a keepalive every 30s, so silence means a dead connection.
	idleTimeout = 90 * time.Second
	// dedupeTTL is how long a published hash is remembered so a repeated
	// delivery (seedctl's replay racing a live event, or a failed ack) isn't
	// published twice.
	dedupeTTL = time.Hour
)

// Subscriber follows seedctl's completion stream. at-least-once: a completion
// is acked only after Publish succeeds; anything unacked is replayed by
// seedctl on the next connect.
type Subscriber struct {
	BaseURL    string
	Client     *http.Client
	Pub        Publisher
	MinBackoff time.Duration
	MaxBackoff time.Duration

	connected atomic.Bool
	lastUp    atomic.Int64 // unix nanos of the last moment the stream was up

	mu   sync.Mutex
	seen map[string]time.Time
}

// Connected reports whether the stream is currently open.
func (s *Subscriber) Connected() bool { return s.connected.Load() }

// LastConnected is the last time the stream was known to be up (zero if never).
func (s *Subscriber) LastConnected() time.Time {
	if s.connected.Load() {
		return time.Now()
	}
	n := s.lastUp.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Run follows the stream until ctx is cancelled, reconnecting with capped
// exponential backoff.
func (s *Subscriber) Run(ctx context.Context) error {
	backoff := s.MinBackoff
	for {
		connected, err := s.streamOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			backoff = s.MinBackoff
		}
		slog.Warn("completion_stream_ended", "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.MaxBackoff)
	}
}

// streamOnce runs one connection. connected reports whether the stream was
// established (so Run can reset its backoff).
func (s *Subscriber) streamOnce(ctx context.Context) (connected bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/v1/completions/stream", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stream: status %d", resp.StatusCode)
	}
	s.connected.Store(true)
	defer func() {
		s.connected.Store(false)
		s.lastUp.Store(time.Now().UnixNano())
	}()
	slog.Info("completion_stream_connected")

	// watchdog: any line read (data or keepalive) pushes the deadline out.
	idle := time.AfterFunc(idleTimeout, cancel)
	defer idle.Stop()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var event, data string
	for sc.Scan() {
		idle.Reset(idleTimeout)
		line := sc.Text()
		switch {
		case line == "":
			if event == "completion" && data != "" {
				if err := s.handle(ctx, data); err != nil {
					return true, err
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, errors.New("stream closed by server")
}

// handle publishes (unless recently published) and acks one completion. a
// publish error is returned so the stream reconnects and seedctl replays;
// an ack error is only logged — the next replay re-acks.
func (s *Subscriber) handle(ctx context.Context, data string) error {
	var c api.Completion
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		slog.Warn("completion_bad_event", "err", err)
		return nil
	}
	if !s.recentlyPublished(c.InfoHash) {
		if err := s.Pub.Publish(ctx, c); err != nil {
			return fmt.Errorf("publish %s: %w", c.InfoHash, err)
		}
		s.markPublished(c.InfoHash)
		slog.Info("completion_published", "hash", c.InfoHash, "name", c.Name)
	}
	if err := s.ack(ctx, c.InfoHash); err != nil {
		slog.Warn("completion_ack_failed", "hash", c.InfoHash, "err", err)
	}
	return nil
}

func (s *Subscriber) ack(ctx context.Context, hash string) error {
	body, err := json.Marshal(api.CompletionAckRequest{Hashes: []string{hash}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/v1/completions/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ack: status %d", resp.StatusCode)
	}
	return nil
}

func (s *Subscriber) recentlyPublished(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.seen[hash]
	return ok && time.Since(t) < dedupeTTL
}

func (s *Subscriber) markPublished(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	now := time.Now()
	for h, t := range s.seen {
		if now.Sub(t) >= dedupeTTL {
			delete(s.seen, h)
		}
	}
	s.seen[hash] = now
}
