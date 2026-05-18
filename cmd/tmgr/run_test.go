package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_HappyPath builds a fake rss server and a fake seedctl, points Run()
// at them, and asserts: matched item is POSTed, unmatched is skipped, dedup
// short-circuit on 200 from GET /v1/torrents/{hash}.
func TestRun_HappyPath(t *testing.T) {
	feedXMLRaw, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	// fakeTorrentBytes is a minimal stand-in — seedctl stub accepts anything.
	fakeTorrentBytes := []byte("fake-torrent-content")

	// feedSrv serves both the rss xml (with rewritten enclosure URLs) and torrent downloads.
	var feedSrvURL string
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "pass=abc", r.Header.Get("Cookie"))
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(fakeTorrentBytes)
			return
		}
		// rewrite enclosure URLs to point at this test server
		rewritten := strings.ReplaceAll(string(feedXMLRaw),
			"https://tracker.example/torrent/dl/",
			feedSrvURL+"/dl/")
		_, _ = w.Write([]byte(rewritten))
	}))
	defer feedSrv.Close()
	feedSrvURL = feedSrv.URL

	var posted []string
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/torrents/"):
			// pretend none are present
			http.Error(w, "nf", 404)
		case r.Method == "POST" && r.URL.Path == "/v1/torrents":
			posted = append(posted, r.Header.Get("X-Info-Hash"))
			_, _ = w.Write([]byte(`{"info_hash":"X","added":true}`))
		default:
			http.Error(w, "bad", 400)
		}
	}))
	defer seedctl.Close()

	cfg := Config{
		SeedctlURL: seedctl.URL,
		Feeds:      []Feed{{Name: "test-feed-a", URL: feedSrv.URL, Cookie: "pass=abc"}},
		Roots:      Roots{TV: "/tv-root", Movies: "/movies-root"},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "tv", Directory: "severance"},
		},
	}

	// 5s < 30s so the jitter sleep at the top of Run is skipped.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Matched)
	assert.Equal(t, 1, res.Added)
	require.Len(t, posted, 1)
	assert.Contains(t, posted[0], "AABBCCDDEEFF") // hash extracted from feed
}

// TestRun_DedupSkip verifies that when seedctl returns 200 for GET /v1/torrents/{hash}
// the item is not re-added.
func TestRun_DedupSkip(t *testing.T) {
	feedXML, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(feedXML)
	}))
	defer feedSrv.Close()

	var posted []string
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/torrents/"):
			// pretend all are already present
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"seeding"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/torrents":
			posted = append(posted, r.Header.Get("X-Info-Hash"))
			_, _ = w.Write([]byte(`{"info_hash":"X","added":true}`))
		default:
			http.Error(w, "bad", 400)
		}
	}))
	defer seedctl.Close()

	cfg := Config{
		SeedctlURL: seedctl.URL,
		Feeds:      []Feed{{Name: "test-feed-a", URL: feedSrv.URL, Cookie: "pass=abc"}},
		Roots:      Roots{TV: "/tv-root"},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "tv", Directory: "severance"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Matched)
	assert.Equal(t, 0, res.Added) // dedup skipped the add
	assert.Len(t, posted, 0)      // no POST made
}

// TestRun_PerFeedCookieIsolation verifies that when two feeds are configured,
// only the feed with a cookie sends a Cookie header. the feed without a cookie
// must NOT send any Cookie header.
func TestRun_PerFeedCookieIsolation(t *testing.T) {
	feedXMLRaw, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	fakeTorrentBytes := []byte("fake-torrent-content")

	// cookieSrv: expects the Cookie header to be present.
	var cookieSrvURL string
	cookieSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "tracker_a=secret", r.Header.Get("Cookie"),
			"cookie feed should always send its cookie")
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(fakeTorrentBytes)
			return
		}
		rewritten := strings.ReplaceAll(string(feedXMLRaw),
			"https://tracker.example/torrent/dl/",
			cookieSrvURL+"/dl/")
		_, _ = w.Write([]byte(rewritten))
	}))
	defer cookieSrv.Close()
	cookieSrvURL = cookieSrv.URL

	// noCookieSrv: must NOT receive any Cookie header.
	var noCookieSrvURL string
	noCookieSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Cookie"),
			"no-cookie feed must not send a Cookie header")
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(fakeTorrentBytes)
			return
		}
		rewritten := strings.ReplaceAll(string(feedXMLRaw),
			"https://tracker.example/torrent/dl/",
			noCookieSrvURL+"/dl/")
		_, _ = w.Write([]byte(rewritten))
	}))
	defer noCookieSrv.Close()
	noCookieSrvURL = noCookieSrv.URL

	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/torrents/"):
			http.Error(w, "nf", 404)
		case r.Method == "POST" && r.URL.Path == "/v1/torrents":
			_, _ = w.Write([]byte(`{"info_hash":"X","added":true}`))
		default:
			http.Error(w, "bad", 400)
		}
	}))
	defer seedctl.Close()

	cfg := Config{
		SeedctlURL: seedctl.URL,
		Feeds: []Feed{
			{Name: "test-feed-a", URL: cookieSrv.URL, Cookie: "tracker_a=secret"},
			{Name: "test-feed-b", URL: noCookieSrv.URL, Cookie: ""},
		},
		Roots: Roots{TV: "/tv-root"},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "tv", Directory: "severance"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}})
	require.NoError(t, err)
	// both feeds match one item each, but they share the same hash → second feed's item
	// will be deduped by seedctl returning 200 on second query OR both get added.
	// what matters: matched=2 (one per feed), errors=0.
	assert.Equal(t, 2, res.Matched)
	assert.Equal(t, 0, res.Errors)
}

// TestRun_TvCategoryComputesSaveDir asserts that a tv category item with a valid
// SxxExx title produces the correct X-Save-Directory header on the POST.
func TestRun_TvCategoryComputesSaveDir(t *testing.T) {
	feedXMLRaw, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	fakeTorrentBytes := []byte("fake-torrent-content")

	var feedSrvURL string
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(fakeTorrentBytes)
			return
		}
		rewritten := strings.ReplaceAll(string(feedXMLRaw),
			"https://tracker.example/torrent/dl/",
			feedSrvURL+"/dl/")
		_, _ = w.Write([]byte(rewritten))
	}))
	defer feedSrv.Close()
	feedSrvURL = feedSrv.URL

	var capturedSaveDir string
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/torrents/"):
			http.Error(w, "nf", 404)
		case r.Method == "POST" && r.URL.Path == "/v1/torrents":
			capturedSaveDir = r.Header.Get("X-Save-Directory")
			_, _ = w.Write([]byte(`{"info_hash":"X","added":true}`))
		default:
			http.Error(w, "bad", 400)
		}
	}))
	defer seedctl.Close()

	cfg := Config{
		SeedctlURL: seedctl.URL,
		Feeds:      []Feed{{Name: "test-feed-a", URL: feedSrv.URL}},
		Roots:      Roots{TV: "/tv-root"},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "tv", Directory: "severance"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Added)
	assert.Equal(t, "/tv-root/severance/season2", capturedSaveDir)
}

// TestRun_MoviesCategoryUsesMoviesRoot asserts that a movies category item
// gets the movies root as its X-Save-Directory.
func TestRun_MoviesCategoryUsesMoviesRoot(t *testing.T) {
	feedXMLRaw, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	fakeTorrentBytes := []byte("fake-torrent-content")

	var feedSrvURL string
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(fakeTorrentBytes)
			return
		}
		rewritten := strings.ReplaceAll(string(feedXMLRaw),
			"https://tracker.example/torrent/dl/",
			feedSrvURL+"/dl/")
		_, _ = w.Write([]byte(rewritten))
	}))
	defer feedSrv.Close()
	feedSrvURL = feedSrv.URL

	var capturedSaveDir string
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/torrents/"):
			http.Error(w, "nf", 404)
		case r.Method == "POST" && r.URL.Path == "/v1/torrents":
			capturedSaveDir = r.Header.Get("X-Save-Directory")
			_, _ = w.Write([]byte(`{"info_hash":"X","added":true}`))
		default:
			http.Error(w, "bad", 400)
		}
	}))
	defer seedctl.Close()

	cfg := Config{
		SeedctlURL: seedctl.URL,
		Feeds:      []Feed{{Name: "test-feed-a", URL: feedSrv.URL}},
		Roots:      Roots{TV: "/tv-root", Movies: "/movies-root"},
		Shows: []feed.ShowRule{
			// match the severance item but treat it as a movie for this test
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "movies"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Added)
	assert.Equal(t, "/movies-root", capturedSaveDir)
}

// TestRun_ObserveMode verifies that in observe mode the pipeline does NOT
// touch the torrent download URL and does NOT call seedctl at all — it only
// logs what it would do. Matched items are still counted.
func TestRun_ObserveMode(t *testing.T) {
	feedXML, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	var torrentDownloadHits int32
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			torrentDownloadHits++
			http.Error(w, "must not be called", 500)
			return
		}
		_, _ = w.Write(feedXML)
	}))
	defer feedSrv.Close()

	var seedctlHits int32
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seedctlHits++
		http.Error(w, "must not be called", 500)
	}))
	defer seedctl.Close()

	cfg := Config{
		Mode:       ModeObserve,
		SeedctlURL: seedctl.URL,
		Feeds:      []Feed{{Name: "obs-feed", URL: feedSrv.URL, Cookie: "pass=abc"}},
		Roots:      Roots{TV: "/tv-root"},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "tv", Directory: "severance"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{})
	require.NoError(t, err)
	assert.Greater(t, res.Matched, 0, "should still recognise matches")
	assert.Equal(t, 0, res.Queried, "must not query seedctl in observe mode")
	assert.Equal(t, 0, res.Added)
	assert.Zero(t, torrentDownloadHits, "must not download torrent bytes in observe mode")
	assert.Zero(t, seedctlHits, "must not contact seedctl in observe mode")
}

// TestRun_FetchMode verifies that fetch mode performs the dedup-check against
// seedctl but NEVER POSTs a torrent. Torrent bytes ARE downloaded so the hash
// can be computed when the feed doesn't supply one.
func TestRun_FetchMode(t *testing.T) {
	feedXMLRaw, err := os.ReadFile("../../internal/feed/testdata/valid.xml")
	require.NoError(t, err)

	var feedSrvURL string
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".torrent") {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write([]byte("fake-torrent-content"))
			return
		}
		rewritten := strings.ReplaceAll(string(feedXMLRaw),
			"https://tracker.example/torrent/dl/",
			feedSrvURL+"/dl/")
		_, _ = w.Write([]byte(rewritten))
	}))
	defer feedSrv.Close()
	feedSrvURL = feedSrv.URL

	var gets, posts int32
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/torrents/"):
			gets++
			http.Error(w, "nf", 404) // not yet present
		case r.Method == "POST":
			posts++
			http.Error(w, "must not POST in fetch mode", 500)
		default:
			http.Error(w, "bad", 400)
		}
	}))
	defer seedctl.Close()

	cfg := Config{
		Mode:       ModeFetch,
		SeedctlURL: seedctl.URL,
		Feeds:      []Feed{{Name: "fetch-feed", URL: feedSrv.URL, Cookie: "pass=abc"}},
		Roots:      Roots{TV: "/tv-root"},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`), Category: "tv", Directory: "severance"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{})
	require.NoError(t, err)
	assert.Greater(t, res.Matched, 0)
	assert.Equal(t, res.Matched, res.Queried, "fetch mode must dedup-check every matched item")
	assert.Equal(t, 0, res.Added)
	assert.Greater(t, gets, int32(0), "seedctl GET must be called")
	assert.Zero(t, posts, "seedctl POST must NOT be called in fetch mode")
}

// TestRun_FeedLogsUseName verifies that when a named feed fails to fetch,
// the error log record carries the feed's Name rather than the raw URL.
func TestRun_FeedLogsUseName(t *testing.T) {
	// feed server always returns 500 so feed.Fetch fails
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer feedSrv.Close()

	// capture slog output into a buffer
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	orig := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(orig) })

	cfg := Config{
		SeedctlURL: "http://127.0.0.1:1", // unreachable; never reached
		Feeds:      []Feed{{Name: "my-named-feed", URL: feedSrv.URL}},
		Shows:      []feed.ShowRule{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg, &http.Client{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Errors)

	logged := buf.String()
	assert.Contains(t, logged, "my-named-feed", "error log should contain the feed name")
	assert.NotContains(t, logged, feedSrv.URL, "error log should not contain the raw URL when a name is set")
}
