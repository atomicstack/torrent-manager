package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/atomicstack/torrent-manager/internal/aiclassify"
	"github.com/atomicstack/torrent-manager/internal/feed"
)

// Feed holds the URL and optional cookie for a single RSS feed.
type Feed struct {
	URL    string
	Cookie string // empty = no Cookie header sent
}

// Roots holds the base filesystem paths for each media category.
type Roots struct {
	TV     string
	Movies string
}

// Config holds the runtime parameters for a single Run tick.
type Config struct {
	SeedctlURL   string
	Feeds        []Feed
	Shows        []feed.ShowRule
	Roots        Roots
	AnthropicKey string // empty = no AI fallback
}

// RunResult summarises what happened during one tick.
type RunResult struct {
	Feeds   int
	Items   int
	Matched int
	Queried int
	Added   int
	Errors  int
}

// Run executes one tick: poll every feed, match items, ask seedctl, and post
// new .torrent bytes. Returns a non-nil error only for configuration errors;
// per-feed / per-item failures are logged and counted into Errors but do not
// abort the run.
func Run(ctx context.Context, cfg Config, hc *http.Client) (RunResult, error) {
	var r RunResult
	r.Feeds = len(cfg.Feeds)

	// startup jitter — skipped if context already has a deadline shorter than 30s
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > 30*time.Second {
		jitter := time.Duration(rand.Intn(30_000)) * time.Millisecond
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(jitter):
		}
	}

	for _, f := range cfg.Feeds {
		items, err := feed.Fetch(ctx, f.URL, f.Cookie)
		if err != nil {
			slog.Error("feed_fetch", "feed", f.URL, "err", err)
			r.Errors++
			continue
		}
		r.Items += len(items)
		for _, it := range items {
			rule, ok := feed.Match(it.Title, cfg.Shows)
			if !ok {
				continue
			}
			r.Matched++
			if err := processItem(ctx, cfg, hc, f.Cookie, it, rule, &r); err != nil {
				slog.Error("process_item", "title", it.Title, "err", err)
				r.Errors++
			}
		}
	}
	slog.Info("run_summary",
		"feeds", r.Feeds, "items", r.Items, "matched", r.Matched,
		"queried", r.Queried, "added", r.Added, "errors", r.Errors)
	return r, nil
}

func processItem(ctx context.Context, cfg Config, hc *http.Client, feedCookie string, it feed.Item, rule feed.ShowRule, r *RunResult) error {
	hash, ok := feed.ExtractHash(it)
	var torrentBytes []byte
	if !ok {
		var err error
		torrentBytes, err = fetchBytes(ctx, hc, it.Enclosure, feedCookie)
		if err != nil {
			return fmt.Errorf("fetch torrent: %w", err)
		}
		hash, err = feed.ComputeHashFromTorrentBytes(torrentBytes)
		if err != nil {
			return fmt.Errorf("compute hash: %w", err)
		}
	}

	// compute save directory based on category
	saveDir := ""
	switch rule.Category {
	case "tv":
		show, season := resolveShowSeason(ctx, cfg, it.Title, rule)
		if show != "" && season > 0 {
			saveDir = filepath.Join(cfg.Roots.TV, show, fmt.Sprintf("season%d", season))
		} else {
			slog.Warn("tv_save_dir_skipped", "title", it.Title, "rule", rule.Name)
		}
	case "movies":
		if cfg.Roots.Movies != "" {
			saveDir = cfg.Roots.Movies
		}
	}

	r.Queried++
	exists, err := seedctlGetStatus(ctx, hc, cfg.SeedctlURL, hash)
	if err != nil {
		return fmt.Errorf("seedctl status: %w", err)
	}
	if exists {
		slog.Info("skip_already_present", "title", it.Title, "hash", hash, "rule", rule.Name)
		return nil
	}
	if torrentBytes == nil {
		torrentBytes, err = fetchBytes(ctx, hc, it.Enclosure, feedCookie)
		if err != nil {
			return fmt.Errorf("fetch torrent: %w", err)
		}
	}
	if err := seedctlAdd(ctx, hc, cfg.SeedctlURL, hash, torrentBytes, saveDir); err != nil {
		return fmt.Errorf("seedctl add: %w", err)
	}
	r.Added++
	slog.Info("added", "title", it.Title, "hash", hash, "rule", rule.Name, "save_dir", saveDir)
	return nil
}

// resolveShowSeason returns the show subdirectory name + season number for a TV item.
// Order:
//  1. season from regex (feed.ExtractSeason); show from rule.Directory (or normalised rule.Name)
//  2. if regex fails AND cfg.AnthropicKey is set: try claude
//  3. otherwise return ("", 0) — caller logs + skips dir override
func resolveShowSeason(ctx context.Context, cfg Config, title string, rule feed.ShowRule) (string, int) {
	show := rule.Directory
	if show == "" {
		show = normaliseShow(rule.Name)
	}
	if season, ok := feed.ExtractSeason(title); ok {
		return show, season
	}
	if cfg.AnthropicKey == "" {
		return "", 0
	}
	aictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := aiclassify.New(cfg.AnthropicKey).Classify(aictx, title)
	if err != nil {
		slog.Warn("aiclassify_failed", "title", title, "err", err)
		return "", 0
	}
	slog.Info("aiclassify_hit", "title", title, "show", res.Show, "season", res.Season)
	// prefer the user-curated directory over claude's show name
	if show == "" {
		show = res.Show
	}
	return show, res.Season
}

// normaliseShow converts a human show name to lowercase dot-separated form.
// Non-alphanumeric runs are collapsed to a single dot; leading/trailing dots
// are trimmed.
func normaliseShow(s string) string {
	var b strings.Builder
	inSep := true // start true so leading separators are trimmed
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
	result := b.String()
	return strings.TrimRight(result, ".")
}

// normaliseShowRegexp is a compiled version for use in tests.
var normaliseShowRegexp = regexp.MustCompile(`[^a-z0-9]+`)

func fetchBytes(ctx context.Context, hc *http.Client, url, cookie string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func seedctlGetStatus(ctx context.Context, hc *http.Client, base, hash string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/torrents/"+hash, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}
}

func seedctlAdd(ctx context.Context, hc *http.Client, base, hash string, body []byte, saveDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/torrents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", hash)
	if saveDir != "" {
		req.Header.Set("X-Save-Directory", saveDir)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}
