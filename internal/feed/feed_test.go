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
	assert.Contains(t, items[0].Enclosure, ".torrent")
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

func TestMatch_FirstWins(t *testing.T) {
	shows := []feed.ShowRule{
		{Name: "specific", Regex: regexp.MustCompile(`(?i)Severance.*1080p`)},
		{Name: "broad", Regex: regexp.MustCompile(`(?i).*1080p.*`)},
	}
	rule, ok := feed.Match("Severance.S02E01.1080p.WEB-DL", shows)
	assert.True(t, ok)
	assert.Equal(t, "specific", rule.Name)
}

func TestMatch_NoMatch(t *testing.T) {
	shows := []feed.ShowRule{
		{Name: "x", Regex: regexp.MustCompile(`(?i)WontMatch`)},
	}
	_, ok := feed.Match("Severance.S02E01.1080p.WEB-DL", shows)
	assert.False(t, ok)
}
