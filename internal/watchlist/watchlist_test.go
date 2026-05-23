package watchlist_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/watchlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeYAML writes body to a fresh tempdir file and returns its path.
func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "watchlist.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadAndCompile_HappyPath(t *testing.T) {
	p := writeYAML(t, `
seedctl_url: https://seedctl.local
roots:
  tv: /srv/tv
  movies: /srv/movies
shows:
  - name: foo
    regex: '(?i)foo\.s\d+e\d+'
    category: tv
    feeds: [tracker_a]
feeds:
  - name: tracker_a
    url: https://tracker.example/rss
`)

	f, err := watchlist.Load(p)
	require.NoError(t, err)
	assert.Equal(t, "https://seedctl.local", f.SeedctlURL)
	assert.Equal(t, "/srv/tv", f.Roots.TV)
	require.Len(t, f.Shows, 1)
	require.Len(t, f.Feeds, 1)

	shows, titleExtracts, err := watchlist.Compile(f)
	require.NoError(t, err)
	require.Len(t, shows, 1)
	assert.Equal(t, "foo", shows[0].Name)
	require.NotNil(t, shows[0].Regex)
	assert.True(t, shows[0].Regex.MatchString("Foo.S01E02"))
	require.Len(t, titleExtracts, 1)
	assert.Nil(t, titleExtracts[0])
}

func TestCompile_ShowReferencesUnknownFeed(t *testing.T) {
	p := writeYAML(t, `
shows:
  - name: foo
    regex: 'foo'
    feeds: [nope]
feeds:
  - name: tracker_a
    url: https://tracker.example/rss
`)
	f, err := watchlist.Load(p)
	require.NoError(t, err)

	_, _, err = watchlist.Compile(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown feed")
}

func TestCompile_BadShowRegex(t *testing.T) {
	p := writeYAML(t, `
shows:
  - name: bad
    regex: '['
feeds:
  - name: tracker_a
    url: https://tracker.example/rss
`)
	f, err := watchlist.Load(p)
	require.NoError(t, err)

	_, _, err = watchlist.Compile(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad regex")
}

func TestCompile_BadTitleExtract(t *testing.T) {
	p := writeYAML(t, `
shows: []
feeds:
  - name: tracker_a
    url: https://tracker.example/rss
    title_extract: '['
`)
	f, err := watchlist.Load(p)
	require.NoError(t, err)

	_, _, err = watchlist.Compile(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad title_extract")
}

func TestCompile_TitleExtractWrongGroupCount(t *testing.T) {
	cases := []struct {
		name string
		re   string
	}{
		{"zero groups", `foo`},
		{"two groups", `(foo)(bar)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeYAML(t, `
shows: []
feeds:
  - name: tracker_a
    url: https://tracker.example/rss
    title_extract: '`+tc.re+`'
`)
			f, err := watchlist.Load(p)
			require.NoError(t, err)

			_, _, err = watchlist.Compile(f)
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "capture groups"),
				"want %q to contain 'capture groups'", err.Error())
		})
	}
}

func TestCompile_TitleExtractOptional(t *testing.T) {
	p := writeYAML(t, `
shows: []
feeds:
  - name: tracker_a
    url: https://tracker.example/rss
  - name: tracker_b
    url: https://other.example/rss
    title_extract: '^(.+)$'
`)
	f, err := watchlist.Load(p)
	require.NoError(t, err)

	_, titleExtracts, err := watchlist.Compile(f)
	require.NoError(t, err)
	require.Len(t, titleExtracts, 2)
	assert.Nil(t, titleExtracts[0], "tracker_a has no title_extract")
	require.NotNil(t, titleExtracts[1], "tracker_b has title_extract set")
	assert.Equal(t, 1, titleExtracts[1].NumSubexp())
}
