// Package savedir derives the rtorrent save directory for a matched feed item.
// Used by tmgr today; phase 2 will share it with tmweb so the two paths agree.
package savedir

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/atomicstack/torrent-manager/internal/feed"
)

// Roots holds the base directories for each category.
type Roots struct {
	TV     string
	Movies string
}

// Derive returns the save directory for an item matching rule. The second
// return value is false when the directory cannot be derived (e.g. tv item
// with no parseable season). Caller should skip the save_dir header in that
// case so rtorrent uses its default download dir.
//
// The function is pure: no network, no logging, no anthropic client. The AI
// fallback in cmd/tmgr/run.go stays at the call site (it's the only consumer
// that has the key) and is layered on top of this function.
func Derive(roots Roots, rule feed.ShowRule, title string) (string, bool) {
	switch rule.Category {
	case "tv":
		if roots.TV == "" {
			return "", false
		}
		show := rule.Directory
		if show == "" {
			show = NormaliseShow(rule.Name)
		}
		season, ok := feed.ExtractSeason(title)
		if !ok || show == "" {
			return "", false
		}
		return filepath.Join(roots.TV, show, fmt.Sprintf("season%d", season)), true
	case "movies":
		if roots.Movies == "" {
			return "", false
		}
		return roots.Movies, true
	default:
		return "", false
	}
}

// NormaliseShow converts a human show name to lowercase dot-separated form.
// Non-alphanumeric runs collapse to a single dot; leading/trailing dots
// trimmed. Exported so callers can preview the derived dir.
func NormaliseShow(s string) string {
	var b strings.Builder
	inSep := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			inSep = false
		} else {
			if !inSep {
				b.WriteByte('.')
				inSep = true
			}
		}
	}
	return strings.TrimRight(b.String(), ".")
}
