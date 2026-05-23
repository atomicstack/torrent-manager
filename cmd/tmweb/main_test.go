package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// muxFor builds a test mux against the given seedctl base url + http client.
// Centralised so we don't repeat the renderer + feeds plumbing in every
// test. Feeds defaults to nil — tests that exercise the dropdown override.
// Shows + roots + cookieEnvs default to zero values; tests that exercise
// the enqueue flow build their own mux via muxForEnqueue.
func muxFor(t *testing.T, base string, hc *http.Client, feeds []string) http.Handler {
	t.Helper()
	r, err := newRenderer()
	require.NoError(t, err)
	return newMux(NewSeedctl(base, hc), feeds, nil, savedir.Roots{}, nil, r)
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

// fooRule returns a canonical "foo" tv rule used by the enqueue tests below.
// Centralised so the matcher and the expected save_dir computation agree.
func fooRule() feed.ShowRule {
	return feed.ShowRule{
		Name:      "foo",
		Category:  "tv",
		Directory: "foo",
		Regex:     regexp.MustCompile(`(?i)foo\.s\d+e\d+`),
	}
}

// sha1Upper returns the uppercase hex sha1 of b. Used to assert the info
// hash that tmweb forwards to seedctl matches the info dict bytes.
func sha1Upper(b []byte) string {
	s := sha1.Sum(b)
	return strings.ToUpper(hex.EncodeToString(s[:]))
}

// TestEnqueueConfirm_RenderModal exercises the happy-path: lookup the item
// by feed+guid, re-derive the save_dir from the watchlist rule, and render
// the modal fragment with all the expected bits in place.
func TestEnqueueConfirm_RenderModal(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{
					FeedName:    "TrackerA (TV)",
					GUID:        "g1",
					Title:       "Foo.S01E01.1080p",
					MatchedRule: "foo",
					LastSeenAt:  time.Now().Unix(),
				},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stub.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerA (TV)"},
		[]feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"},
		nil,
		r,
	)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerA%20(TV)&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Foo.S01E01.1080p", "modal shows the item title")
	assert.Contains(t, body, "/t/tv/foo/season1", "save_dir derived correctly")
	assert.Contains(t, body, `hx-post="/api/enqueue`, "confirm button POSTs back to /api/enqueue")
}

// TestEnqueueConfirm_NoRule covers the case where the item has no matched
// rule (phase 3 will fill this with a manual flow). The button on the row
// is disabled but if someone forces the request directly we still render a
// useful modal — not silently dismiss.
func TestEnqueueConfirm_NoRule(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
			{FeedName: "TrackerA (TV)", GUID: "g1", Title: "Random.Release.1080p", LastSeenAt: time.Now().Unix()},
		}})
	}))
	t.Cleanup(stub.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerA (TV)"}, []feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"}, nil, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerA%20(TV)&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "no rule")
	assert.Contains(t, body, "phase 3")
}

// TestEnqueueDo_PostsToSeedctlAndRendersTable is the full happy-path
// integration. It stubs:
//  1. a tracker that serves a minimal bencoded .torrent,
//  2. a seedctl that records the headers + body posted to /v1/torrents
//     and replies with the list-items lookup on /v1/items.
//
// Then it POSTs /api/enqueue and asserts:
//   - the correct info hash header was forwarded,
//   - the correct save_dir header was forwarded,
//   - the raw .torrent bytes were forwarded verbatim,
//   - the response body is a fresh table-rows fragment (re-rendered).
func TestEnqueueDo_PostsToSeedctlAndRendersTable(t *testing.T) {
	// Build the .torrent first so we know the hash to assert.
	info := metainfo.Info{
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
		Name:        "foo.mkv",
		Length:      1,
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	torrentBytes, err := bencode.Marshal(mi)
	require.NoError(t, err)
	expectedHash := sha1Upper(infoBytes)

	// Tracker stub: serves the .torrent on any path.
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrentBytes)
	}))
	t.Cleanup(tracker.Close)

	// Seedctl stub: serves the items lookup with Link pointing at our
	// tracker, and records the headers + body it receives on /v1/torrents.
	var seedctlPosted struct {
		hash    string
		saveDir string
		body    []byte
	}
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/items":
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{
					FeedName:    "TrackerA (TV)",
					GUID:        "g1",
					Title:       "Foo.S01E01.1080p",
					MatchedRule: "foo",
					Link:        tracker.URL + "/.torrent",
					LastSeenAt:  time.Now().Unix(),
				},
			}})
		case "/v1/torrents":
			seedctlPosted.hash = r.Header.Get("X-Info-Hash")
			seedctlPosted.saveDir = r.Header.Get("X-Save-Directory")
			seedctlPosted.body, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(api.AddTorrentResponse{
				InfoHash: seedctlPosted.hash,
				Added:    true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(seedctl.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(seedctl.URL, seedctl.Client()),
		[]string{"TrackerA (TV)"},
		[]feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"},
		nil,
		r,
	)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/enqueue?feed=TrackerA%20(TV)&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	assert.Equal(t, expectedHash, seedctlPosted.hash, "info hash forwarded to seedctl")
	assert.Equal(t, "/t/tv/foo/season1", seedctlPosted.saveDir, "save_dir derived correctly")
	assert.Equal(t, torrentBytes, seedctlPosted.body, "raw .torrent bytes forwarded")

	// The response body is a re-rendered table-rows fragment.
	assert.Contains(t, rr.Body.String(), "Foo.S01E01.1080p")
}

// TestEnqueueDo_TrackerFailureRendersErrorRow ensures that if fetching the
// .torrent fails (e.g. cookie missing / 403), we surface the error as a
// table-rows fragment so the user sees the cause inline instead of a
// silent no-op.
func TestEnqueueDo_TrackerFailureRendersErrorRow(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(tracker.Close)

	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{
					FeedName:    "TrackerA (TV)",
					GUID:        "g1",
					Title:       "Foo.S01E01.1080p",
					MatchedRule: "foo",
					Link:        tracker.URL + "/.torrent",
					LastSeenAt:  time.Now().Unix(),
				},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(seedctl.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(seedctl.URL, seedctl.Client()),
		[]string{"TrackerA (TV)"}, []feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"}, nil, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/enqueue?feed=TrackerA%20(TV)&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "enqueue failed")
	assert.Contains(t, body, "status 403")
}
