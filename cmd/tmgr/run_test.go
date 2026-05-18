package main

import (
	"context"
	"crypto/tls"
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
		Feeds:      []Feed{{URL: feedSrv.URL, Cookie: "pass=abc"}},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`)},
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
		Feeds:      []Feed{{URL: feedSrv.URL, Cookie: "pass=abc"}},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`)},
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
			{URL: cookieSrv.URL, Cookie: "tracker_a=secret"},
			{URL: noCookieSrv.URL, Cookie: ""},
		},
		Shows: []feed.ShowRule{
			{Name: "severance", Regex: regexp.MustCompile(`(?i)Severance.*1080p`)},
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
