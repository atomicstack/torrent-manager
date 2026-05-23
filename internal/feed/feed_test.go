package feed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_Valid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, _ := os.ReadFile("testdata/valid.xml")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	items, err := feed.Fetch(context.Background(), srv.URL, "")
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "Severance.S02E01.1080p.WEB-DL.DDP5.1.H.264-GROUP", items[0].Title)
	assert.Contains(t, items[0].Link, ".torrent")
}

func TestParse_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, _ := os.ReadFile("testdata/empty.xml")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	items, err := feed.Fetch(context.Background(), srv.URL, "")
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestParse_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, _ := os.ReadFile("testdata/malformed.xml")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	_, err := feed.Fetch(context.Background(), srv.URL, "")
	require.Error(t, err)
}

func TestFetch_SendsCookie(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		b, _ := os.ReadFile("testdata/valid.xml")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	_, err := feed.Fetch(context.Background(), srv.URL, "pass=abc123")
	require.NoError(t, err)
	assert.Equal(t, "pass=abc123", gotCookie)
}

// TestFetch_NoEnclosureFallsBackToLink: TrackerA-style feeds expose the .torrent
// URL in <link> instead of <enclosure>. Item.Link must end up holding that
// URL so the downstream fetch can find the bytes.
func TestFetch_NoEnclosureFallsBackToLink(t *testing.T) {
	rss := `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title>` +
		`<link>http://x</link><description>x</description>` +
		`<item>` +
		`<title>Some.Show.S01E01.1080p.WEB-DL.mkv</title>` +
		`<link>https://tracker.example/d.php?r=aabbccdd</link>` +
		`</item>` +
		`</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rss))
	}))
	defer srv.Close()

	items, err := feed.Fetch(context.Background(), srv.URL, "")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "https://tracker.example/d.php?r=aabbccdd", items[0].Link,
		"missing <enclosure> should fall back to <link>")
}

// TestFetch_EnclosureWinsOverLink: when both are present, the explicit
// <enclosure> URL takes precedence.
func TestFetch_EnclosureWinsOverLink(t *testing.T) {
	rss := `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title>` +
		`<link>http://x</link><description>x</description>` +
		`<item>` +
		`<title>Severance.S02E01.1080p.WEB-DL.mkv</title>` +
		`<link>https://tracker.example/page/123</link>` +
		`<enclosure url="https://tracker.example/dl/x.torrent" type="application/x-bittorrent" length="0"/>` +
		`</item>` +
		`</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rss))
	}))
	defer srv.Close()

	items, err := feed.Fetch(context.Background(), srv.URL, "")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "https://tracker.example/dl/x.torrent", items[0].Link)
}

func TestMatch_FirstWins(t *testing.T) {
	shows := []feed.ShowRule{
		{Name: "specific", Regex: regexp.MustCompile(`(?i)Severance.*1080p`)},
		{Name: "broad", Regex: regexp.MustCompile(`(?i).*1080p.*`)},
	}
	rule, ok := feed.Match("Severance.S02E01.1080p.WEB-DL", "any-feed", shows)
	assert.True(t, ok)
	assert.Equal(t, "specific", rule.Name)
}

func TestMatch_NoMatch(t *testing.T) {
	shows := []feed.ShowRule{
		{Name: "x", Regex: regexp.MustCompile(`(?i)WontMatch`)},
	}
	_, ok := feed.Match("Severance.S02E01.1080p.WEB-DL", "any-feed", shows)
	assert.False(t, ok)
}

// TestMatch_FeedScopedRuleSkipsOtherFeed: a rule scoped to one feed must not
// fire on a different feed even if the regex matches.
func TestMatch_FeedScopedRuleSkipsOtherFeed(t *testing.T) {
	shows := []feed.ShowRule{
		{
			Name:  "tracker-a-only",
			Regex: regexp.MustCompile(`(?i)last.week.tonight.*1080p`),
			Feeds: []string{"TrackerA (TV)"},
		},
	}
	_, ok := feed.Match("Last Week Tonight S13E12 1080p WEB-DL", "TrackerB (TV)", shows)
	assert.False(t, ok, "rule scoped to TrackerA (TV) must not match an TrackerB feed item")

	rule, ok2 := feed.Match("Last Week Tonight S13E12 1080p WEB-DL", "TrackerA (TV)", shows)
	assert.True(t, ok2, "rule scoped to TrackerA (TV) must match an TrackerA feed item")
	assert.Equal(t, "tracker-a-only", rule.Name)
}

// TestMatch_UnscopedRuleMatchesEveryFeed: a rule with no Feeds list applies
// universally.
func TestMatch_UnscopedRuleMatchesEveryFeed(t *testing.T) {
	shows := []feed.ShowRule{
		{Name: "anywhere", Regex: regexp.MustCompile(`(?i)Severance.*1080p`)},
	}
	_, ok1 := feed.Match("Severance.S02E01.1080p", "feed-a", shows)
	_, ok2 := feed.Match("Severance.S02E01.1080p", "feed-b", shows)
	assert.True(t, ok1)
	assert.True(t, ok2)
}

// TestMatch_MultiFeedScope: a rule scoped to multiple feeds matches any of them.
func TestMatch_MultiFeedScope(t *testing.T) {
	shows := []feed.ShowRule{
		{
			Name:  "two-feeds",
			Regex: regexp.MustCompile(`(?i)Some.Show`),
			Feeds: []string{"feed-a", "feed-b"},
		},
	}
	_, ok1 := feed.Match("Some.Show.S01E01", "feed-a", shows)
	_, ok2 := feed.Match("Some.Show.S01E01", "feed-b", shows)
	_, ok3 := feed.Match("Some.Show.S01E01", "feed-c", shows)
	assert.True(t, ok1)
	assert.True(t, ok2)
	assert.False(t, ok3)
}
