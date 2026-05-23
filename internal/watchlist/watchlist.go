// Package watchlist parses and validates the on-disk yaml schema shared by
// tmgr and tmweb. It is the single source of truth for the watchlist file
// format. The package is intentionally side-effect free: no logging, no
// os.Exit, no global state. Validation failures are returned as wrapped
// errors so the caller can decide how to surface them.
package watchlist

import (
	"fmt"
	"os"
	"regexp"

	"github.com/atomicstack/torrent-manager/internal/feed"
	"gopkg.in/yaml.v3"
)

// File is the on-disk yaml schema.
type File struct {
	SeedctlURL string `yaml:"seedctl_url"`
	Roots      Roots  `yaml:"roots"`
	Shows      []Show `yaml:"shows"`
	Feeds      []Feed `yaml:"feeds"`
}

// Roots holds the per-category base directories used to derive the rtorrent
// save_dir.
type Roots struct {
	TV     string `yaml:"tv"`
	Movies string `yaml:"movies"`
}

// Show is the yaml representation of one show rule, pre-regex-compile.
type Show struct {
	Name      string   `yaml:"name"`
	Regex     string   `yaml:"regex"`
	Category  string   `yaml:"category,omitempty"`
	Directory string   `yaml:"directory,omitempty"`
	Feeds     []string `yaml:"feeds,omitempty"`
}

// Feed is the yaml representation of one feed, pre-regex-compile.
type Feed struct {
	Name         string `yaml:"name,omitempty"`
	URL          string `yaml:"url"`
	CookieEnv    string `yaml:"cookie_env,omitempty"`
	Category     string `yaml:"category,omitempty"`
	TitleExtract string `yaml:"title_extract,omitempty"`
}

// Load reads, parses, and validates the watchlist at path. Returns rich
// errors (no logging side-effects) so callers can control how validation
// failures surface.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read watchlist %q: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse watchlist %q: %w", path, err)
	}
	return &f, nil
}

// Compile turns the on-disk File into runtime feed.ShowRule + compiled
// title_extract regexes ready for tmgr's pipeline.
//
// Returns:
//   - shows: slice of feed.ShowRule with regex compiled and Feeds validated.
//   - titleExtracts: parallel slice (1:1 with File.Feeds) of *regexp.Regexp
//     or nil when no title_extract is set on that feed.
//
// Validation errors:
//   - bad regex on a show:                "bad regex in show %q: %w"
//   - show references unknown feed:       "show %q references unknown feed %q"
//   - title_extract bad regex:            "bad title_extract in feed %q: %w"
//   - title_extract wrong subexpr count:  "title_extract in feed %q has %d capture groups (want 1)"
func Compile(f *File) (shows []feed.ShowRule, titleExtracts []*regexp.Regexp, err error) {
	feedNames := make(map[string]struct{}, len(f.Feeds))
	for _, ff := range f.Feeds {
		if ff.Name != "" {
			feedNames[ff.Name] = struct{}{}
		}
	}

	shows = make([]feed.ShowRule, 0, len(f.Shows))
	for _, s := range f.Shows {
		re, rerr := regexp.Compile(s.Regex)
		if rerr != nil {
			return nil, nil, fmt.Errorf("bad regex in show %q: %w", s.Name, rerr)
		}
		for _, fn := range s.Feeds {
			if _, ok := feedNames[fn]; !ok {
				return nil, nil, fmt.Errorf("show %q references unknown feed %q", s.Name, fn)
			}
		}
		shows = append(shows, feed.ShowRule{
			Name:      s.Name,
			Regex:     re,
			Category:  s.Category,
			Directory: s.Directory,
			Feeds:     s.Feeds,
		})
	}

	titleExtracts = make([]*regexp.Regexp, len(f.Feeds))
	for i, ff := range f.Feeds {
		if ff.TitleExtract == "" {
			continue
		}
		re, rerr := regexp.Compile(ff.TitleExtract)
		if rerr != nil {
			return nil, nil, fmt.Errorf("bad title_extract in feed %q: %w", ff.Name, rerr)
		}
		if n := re.NumSubexp(); n != 1 {
			return nil, nil, fmt.Errorf("title_extract in feed %q has %d capture groups (want 1)", ff.Name, n)
		}
		titleExtracts[i] = re
	}

	return shows, titleExtracts, nil
}
