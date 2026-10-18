package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePublisher struct {
	mu        sync.Mutex
	published []api.Completion
	failNext  int // fail this many calls first
}

func (f *fakePublisher) Publish(_ context.Context, c api.Completion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("broker down")
	}
	f.published = append(f.published, c)
	return nil
}

func (f *fakePublisher) hashes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.published {
		out = append(out, c.InfoHash)
	}
	return out
}

// fakeSeedctl streams every unacked completion on connect (like seedctl's
// replay), then holds the stream open. acks remove entries.
type fakeSeedctl struct {
	mu       sync.Mutex
	pending  []api.Completion
	acked    []string
	connects atomic.Int32
	extra    []api.Completion // sent after the replay on every connect
}

func (s *fakeSeedctl) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/completions/stream", func(w http.ResponseWriter, r *http.Request) {
		s.connects.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		s.mu.Lock()
		send := append(append([]api.Completion{}, s.pending...), s.extra...)
		s.mu.Unlock()
		_, _ = fmt.Fprint(w, ": connected\n\n")
		for _, c := range send {
			b, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "event: completion\ndata: %s\n\n", b)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /v1/completions/ack", func(w http.ResponseWriter, r *http.Request) {
		var req api.CompletionAckRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, h := range req.Hashes {
			s.acked = append(s.acked, h)
			for i, c := range s.pending {
				if c.InfoHash == h {
					s.pending = append(s.pending[:i], s.pending[i+1:]...)
					break
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (s *fakeSeedctl) ackedHashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.acked...)
}

func runSubscriber(t *testing.T, seed *fakeSeedctl, pub Publisher) {
	t.Helper()
	srv := httptest.NewServer(seed.handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	sub := &Subscriber{
		BaseURL: srv.URL, Client: srv.Client(), Pub: pub,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = sub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestSubscriber_PublishesThenAcks(t *testing.T) {
	seed := &fakeSeedctl{pending: []api.Completion{{InfoHash: "AAAA"}, {InfoHash: "BBBB"}}}
	pub := &fakePublisher{}
	runSubscriber(t, seed, pub)

	require.Eventually(t, func() bool { return len(seed.ackedHashes()) == 2 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"AAAA", "BBBB"}, pub.hashes())
	assert.Equal(t, []string{"AAAA", "BBBB"}, seed.ackedHashes())
}

// a publish failure must not ack: the subscriber drops the stream, backs
// off, reconnects, and the replay delivers the completion again.
func TestSubscriber_PublishFailureRetriesViaReplay(t *testing.T) {
	seed := &fakeSeedctl{pending: []api.Completion{{InfoHash: "AAAA"}}}
	pub := &fakePublisher{failNext: 2}
	runSubscriber(t, seed, pub)

	require.Eventually(t, func() bool { return len(seed.ackedHashes()) == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"AAAA"}, pub.hashes())
	assert.GreaterOrEqual(t, seed.connects.Load(), int32(3))
}

// seedctl can deliver the same completion twice (replay racing a live
// event); it's published once but acked each time.
func TestSubscriber_DedupesRepeatedCompletion(t *testing.T) {
	seed := &fakeSeedctl{extra: []api.Completion{{InfoHash: "AAAA"}, {InfoHash: "AAAA"}}}
	pub := &fakePublisher{}
	runSubscriber(t, seed, pub)

	require.Eventually(t, func() bool { return len(seed.ackedHashes()) == 2 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"AAAA"}, pub.hashes())
}

func TestSubscriber_ReconnectsAfterStreamEnds(t *testing.T) {
	var connects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions/stream" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		connects.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": connected\n\n") // then return: stream ends
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := &Subscriber{BaseURL: srv.URL, Client: srv.Client(), Pub: &fakePublisher{},
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	go func() { _ = sub.Run(ctx) }()
	require.Eventually(t, func() bool { return connects.Load() >= 3 }, 2*time.Second, 5*time.Millisecond)
}

func TestMessageFor_RelativisesSaveDir(t *testing.T) {
	c := api.Completion{InfoHash: "AAAA", Name: "x.mkv", Title: "X", SaveDir: "/t/tv/x/season1", FinishedAt: 7}
	b, err := MessageFor(c, "/t/")
	require.NoError(t, err)
	assert.JSONEq(t, `{"info_hash":"AAAA","name":"x.mkv","title":"X","save_dir":"tv/x/season1","finished_at":7}`, string(b))

	b, err = MessageFor(api.Completion{SaveDir: "/elsewhere/x"}, "/t/")
	require.NoError(t, err)
	assert.Contains(t, string(b), `"save_dir":"/elsewhere/x"`)
}
