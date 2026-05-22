package savedir_test

import (
	"regexp"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/stretchr/testify/assert"
)

func TestDerive_TVRuleWithDirectory(t *testing.T) {
	rule := feed.ShowRule{
		Name:      "guy mont spelling bee AU",
		Category:  "tv",
		Directory: "guy.montgomerys.guy.mont.spelling.bee.au",
		Regex:     regexp.MustCompile(`(?i)guy.mont.spelling.bee.au.s[0-9][0-9]e[0-9][0-9]`),
	}
	roots := savedir.Roots{TV: "/t/tv", Movies: "/t/movies"}
	got, ok := savedir.Derive(roots, rule, "Guy.Montgomerys.Guy.Mont.Spelling.Bee.Au.S03E04.1080p.WEB-DL.AAC2.0.H.264-WH.mp4")
	assert.True(t, ok)
	assert.Equal(t, "/t/tv/guy.montgomerys.guy.mont.spelling.bee.au/season3", got)
}

func TestDerive_TVRuleNoSeasonReturnsFalse(t *testing.T) {
	rule := feed.ShowRule{Name: "foo", Category: "tv", Directory: "foo"}
	roots := savedir.Roots{TV: "/t/tv"}
	_, ok := savedir.Derive(roots, rule, "Random.Title.No.Season.Marker.1080p")
	assert.False(t, ok)
}

func TestDerive_MoviesUsesRootOnly(t *testing.T) {
	rule := feed.ShowRule{Name: "blade runner", Category: "movies"}
	roots := savedir.Roots{TV: "/t/tv", Movies: "/t/movies"}
	got, ok := savedir.Derive(roots, rule, "Blade.Runner.2049.2160p.UHD")
	assert.True(t, ok)
	assert.Equal(t, "/t/movies", got)
}

func TestDerive_NormaliseShowFromRuleName(t *testing.T) {
	rule := feed.ShowRule{Name: "Last Week Tonight", Category: "tv"}
	got, ok := savedir.Derive(savedir.Roots{TV: "/t/tv"}, rule, "Last.Week.Tonight.S11E15.1080p")
	assert.True(t, ok)
	assert.Equal(t, "/t/tv/last.week.tonight/season11", got)
}
