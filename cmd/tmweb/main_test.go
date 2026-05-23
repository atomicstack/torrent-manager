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
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/atomicstack/torrent-manager/internal/aiclassify"
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
	return newMux(NewSeedctl(base, hc), feeds, nil, nil, nil, savedir.Roots{}, nil, r)
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
				{FeedName: "TrackerA (TV)", Title: "X", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
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
// enqueued badge.
func TestSearch_RendersTableRows(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
			{FeedName: "TrackerA (TV)", Title: "Search Hit", InfoHash: "DEAD", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix(), Enqueued: true, EnqueueSource: "auto"},
		}})
	}))
	t.Cleanup(stub.Close)

	mux := muxFor(t, stub.URL, stub.Client(), []string{"TrackerA (TV)"})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search?q=hit", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Search Hit")
	assert.Contains(t, body, "badge-enqueued")
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
		nil, nil,
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

// TestEnqueueConfirm_NoRule_OtherCategory covers the "other" branch of the
// no-rule flow: the modal renders with an empty save_dir input (no auto
// proposal, no auto-pick failure note — it's just a freeform field).
func TestEnqueueConfirm_NoRule_OtherCategory(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
			{FeedName: "TrackerA (Other)", GUID: "g1", Title: "Random.Release.1080p", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
		}})
	}))
	t.Cleanup(stub.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerA (Other)"}, nil,
		map[string]string{"TrackerA (Other)": "other"},
		[]feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"}, nil, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerA%20(Other)&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "no watchlist rule matched")
	assert.Contains(t, body, `name="save_dir"`, "must include editable save_dir input")
	assert.Contains(t, body, `value=""`, "no proposed dir for the 'other' category")
}

// TestEnqueueConfirm_NoRule_TV_ClaudeProposes is the live-Claude happy
// path. We can't inject a fake aiclassify.Client without refactoring the
// type, and we don't want unit tests to depend on a live API. The
// AutoPickFailed test below covers the structural path; this skip is a
// placeholder so the intent is documented next to its sibling tests.
func TestEnqueueConfirm_NoRule_TV_ClaudeProposes(t *testing.T) {
	t.Skip("covered by AutoPickFailed test; live ai not exercised in unit tests")
}

// TestEnqueueConfirm_NoRule_TV_AutoPickFailed_RendersEditable covers the
// tv branch when ai is nil (ANTHROPIC_API_KEY unset). The modal renders
// with auto-classify-failed messaging and an empty editable save_dir
// input so the user can fill it in.
func TestEnqueueConfirm_NoRule_TV_AutoPickFailed_RendersEditable(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{FeedName: "TrackerB (TV)", GUID: "g1", Title: "Severance.S02E01.1080p", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
			}})
			return
		}
		if r.URL.Path == "/v1/dirs" {
			_ = json.NewEncoder(w).Encode(api.DirsListResponse{Dirs: []string{"severance"}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stub.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerB (TV)"}, nil,
		map[string]string{"TrackerB (TV)": "tv"},
		nil, savedir.Roots{TV: "/t/tv"},
		nil, // ai nil → auto-pick failed
		r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerB+%28TV%29&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "no watchlist rule matched")
	assert.Contains(t, body, "auto-classify failed")
	assert.Contains(t, body, `name="save_dir"`, "must include editable save_dir input")
	assert.Contains(t, body, `value=""`, "no proposed dir when auto-pick failed")
}

// TestEnqueueConfirm_NoRule_Movies covers the movies branch: the modal
// pre-fills the save_dir with roots.Movies (no subdir convention for
// movies — the user can extend the path if they want a per-film dir).
func TestEnqueueConfirm_NoRule_Movies(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{FeedName: "TrackerB (Movies)", GUID: "m1", Title: "Blade.Runner.2049.2160p"},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stub.Close)
	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerB (Movies)"}, nil,
		map[string]string{"TrackerB (Movies)": "movies"},
		nil, savedir.Roots{Movies: "/t/movies"},
		nil, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerB+%28Movies%29&guid=m1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `value="/t/movies"`, "movies path pre-fills the root")
}

// TestEnqueueDo_NoRule_AcceptsFormSaveDir confirms the POST handler reads
// save_dir from the form (not from any server-side derivation). This is
// what lets the no-rule flow (and a future user-edited rule-matched flow)
// override the proposed dir.
func TestEnqueueDo_NoRule_AcceptsFormSaveDir(t *testing.T) {
	info := metainfo.Info{
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
		Name:        "x.mkv",
		Length:      1,
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	torrentBytes, err := bencode.Marshal(mi)
	require.NoError(t, err)

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrentBytes)
	}))
	t.Cleanup(tracker.Close)

	var posted struct {
		hash    string
		saveDir string
		body    []byte
	}
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/items":
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{FeedName: "TrackerB (TV)", GUID: "g1", Title: "X.S01E01", Link: tracker.URL + "/.torrent", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
			}})
		case "/v1/torrents":
			posted.hash = r.Header.Get("X-Info-Hash")
			posted.saveDir = r.Header.Get("X-Save-Directory")
			posted.body, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(api.AddTorrentResponse{InfoHash: posted.hash, Added: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(seedctl.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(seedctl.URL, seedctl.Client()),
		[]string{"TrackerB (TV)"}, nil,
		map[string]string{"TrackerB (TV)": "tv"},
		nil, savedir.Roots{TV: "/t/tv"},
		nil, r)
	req := httptest.NewRequest(http.MethodPost,
		"/api/enqueue?feed=TrackerB+%28TV%29&guid=g1",
		strings.NewReader("save_dir=/custom/path"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "/custom/path", posted.saveDir, "user-entered save_dir forwarded to seedctl")
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
		nil, nil,
		[]feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"},
		nil,
		r,
	)
	rr := httptest.NewRecorder()
	// the rule-matched modal posts back with the derived save_dir as a
	// hidden input; mirror that here so the handler picks it up via the
	// form field (matches the production flow).
	req := httptest.NewRequest(http.MethodPost,
		"/api/enqueue?feed=TrackerA%20(TV)&guid=g1",
		strings.NewReader("save_dir=/t/tv/foo/season1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	assert.Equal(t, expectedHash, seedctlPosted.hash, "info hash forwarded to seedctl")
	assert.Equal(t, "/t/tv/foo/season1", seedctlPosted.saveDir, "save_dir forwarded from form")
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
		[]string{"TrackerA (TV)"}, nil, nil,
		[]feed.ShowRule{fooRule()},
		savedir.Roots{TV: "/t/tv"}, nil, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/enqueue?feed=TrackerA%20(TV)&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "enqueue failed")
	assert.Contains(t, body, "status 403")
}

// TestEnqueueConfirm_NoRule_TV_RendersLoadingShell verifies the new fast-path
// behaviour from the modal-loading UX change: when ai is configured, the tv
// no-rule branch returns the modal shell *immediately* with a loading
// placeholder + an hx-get pointing at /api/enqueue-classify. The classify
// endpoint is what eventually does the slow claude call.
func TestEnqueueConfirm_NoRule_TV_RendersLoadingShell(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{FeedName: "TrackerB (TV)", GUID: "g1", Title: "Severance.S02E01.1080p", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stub.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	// non-nil ai client — points at an unused stub since enqueue-confirm
	// shouldn't actually invoke claude (that happens in /api/enqueue-classify).
	anthropicStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("claude should NOT be called by enqueue-confirm; only by enqueue-classify")
		w.WriteHeader(500)
	}))
	t.Cleanup(anthropicStub.Close)
	ai := aiclassify.New("test", option.WithBaseURL(anthropicStub.URL))

	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerB (TV)"}, nil,
		map[string]string{"TrackerB (TV)": "tv"},
		nil, savedir.Roots{TV: "/t/tv"},
		ai, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerB+%28TV%29&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "no watchlist rule matched", "modal opens immediately")
	assert.Contains(t, body, "asking claude", "loading placeholder visible")
	assert.Contains(t, body, "classify-loading", "css hook present for the progress bar")
	assert.Regexp(t, regexp.MustCompile(`hx-get="/api/enqueue-classify\?feed=TrackerB[^"]+"`), body,
		"hx-get fires the classify endpoint")
	assert.NotContains(t, body, `name="save_dir"`, "form is NOT inline yet — it gets swapped in later")
}

// TestEnqueueClassify_RendersForm covers the slow path returned to htmx after
// the loading placeholder fires. It exercises the seedctl /v1/dirs round-trip
// + the aiclassify call (against a stubbed anthropic server) and asserts the
// proposed save_dir lands in the rendered form.
func TestEnqueueClassify_RendersForm(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/items":
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{
				{FeedName: "TrackerB (TV)", GUID: "g1", Title: "Severance.S02E01.1080p"},
			}})
		case "/v1/dirs":
			_ = json.NewEncoder(w).Encode(api.DirsListResponse{Dirs: []string{"beef", "severance"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.Close)

	anthropicStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_test",
			"type": "message",
			"role": "assistant",
			"model": "claude-opus-4-7",
			"content": [{"type": "text", "text": "{\"show_dir\": \"severance\", \"season\": 2, \"is_new_dir\": false}"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 50, "output_tokens": 15}
		}`)
	}))
	t.Cleanup(anthropicStub.Close)
	ai := aiclassify.New("test", option.WithBaseURL(anthropicStub.URL))

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(stub.URL, stub.Client()),
		[]string{"TrackerB (TV)"}, nil,
		map[string]string{"TrackerB (TV)": "tv"},
		nil, savedir.Roots{TV: "/t/tv"},
		ai, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-classify?feed=TrackerB+%28TV%29&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `value="/t/tv/severance/season2"`, "claude's proposal lands in the input")
	assert.Contains(t, body, "claude suggested the path below", "notice rendered")
	assert.Contains(t, body, `name="save_dir"`, "form input present")
}
