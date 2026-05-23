// Package feed parses an RSS feed for torrent items and matches their titles
// against a list of user-configured regexes.
package feed

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/mmcdole/gofeed"
)

// Item is the minimum subset of a feed entry we care about.
//
// Link semantically means "the download URL for the .torrent file":
//   - if the feed entry has an <enclosure>, Link = that enclosure URL.
//   - otherwise Link = the entry's <link> element (TrackerA-style feeds with no enclosure).
//
// We deliberately collapse the two into one field because every feed we've
// looked at sets <link> = <enclosure> when both are present; storing them
// separately wasted bytes for no information gain.
type Item struct {
	Title string
	GUID  string
	Link  string
}

// ShowRule is a single watchlist entry.
type ShowRule struct {
	Name      string // human label only — not used in matching
	Regex     *regexp.Regexp
	Category  string   // optional: "tv" | "movies"
	Directory string   // optional: explicit show subdir name (lowercase, dot-separated)
	Feeds     []string // optional: only match when current feed.Name ∈ Feeds. nil/empty = match every feed
}

// Fetch GETs the feed URL with an optional cookie header and returns the
// parsed items. Applies a 10s timeout via context.WithTimeout on top of any
// deadline already on ctx (the shorter wins). The 10s is self-imposed inside
// this function — http.DefaultClient has no top-level Timeout — so a caller
// passing context.Background() will still be bounded by the 10s ceiling here.
func Fetch(ctx context.Context, url, cookie string) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch feed: status %d", resp.StatusCode)
	}
	parsed, err := gofeed.NewParser().Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	out := make([]Item, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		i := Item{Title: it.Title, GUID: it.GUID, Link: it.Link}
		if len(it.Enclosures) > 0 && it.Enclosures[0].URL != "" {
			// prefer <enclosure> when present (well-behaved RSS); fall back to
			// <link> for TrackerA-style feeds that ship the torrent URL there.
			i.Link = it.Enclosures[0].URL
		}
		out = append(out, i)
	}
	return out, nil
}

// Match returns the first ShowRule whose regex matches title AND whose feed
// scope (if any) includes feedName. A rule with empty/nil Feeds applies to
// every feed; otherwise the current feed name must appear in the list (exact,
// case-sensitive). Order matters: callers should list more specific rules first.
func Match(title, feedName string, rules []ShowRule) (ShowRule, bool) {
	for _, r := range rules {
		if !ruleAppliesToFeed(r, feedName) {
			continue
		}
		if r.Regex.MatchString(title) {
			return r, true
		}
	}
	return ShowRule{}, false
}

func ruleAppliesToFeed(r ShowRule, feedName string) bool {
	if len(r.Feeds) == 0 {
		return true
	}
	for _, f := range r.Feeds {
		if f == feedName {
			return true
		}
	}
	return false
}
