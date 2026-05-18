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
	Name   string // human-readable label; falls back to URL in logs if empty
	URL    string
	Cookie string // empty = no Cookie header sent

	// TitleExtract is an optional regex with exactly one capture group. When set,
	// it is applied to each item's <title>; capture-group-1 becomes the canonical
	// title used for rule matching and downstream logging. If the pattern doesn't
	// match a given title, the raw title is used as-is.
	TitleExtract *regexp.Regexp
}

// Roots holds the base filesystem paths for each media category.
type Roots struct {
	TV     string
	Movies string
}

// Mode controls how far the pipeline runs for each matched item. Use it as a
// safety throttle while bringing the deployment up.
type Mode string

const (
	ModeObserve Mode = "observe" // log only; no torrent download, no seedctl traffic
	ModeFetch   Mode = "fetch"   // download torrent + compute hash + dedup-check seedctl; no POST
	ModeEnqueue Mode = "enqueue" // full pipeline (default)
)

// Config holds the runtime parameters for a single Run tick.
type Config struct {
	SeedctlURL   string
	Feeds        []Feed
	Shows        []feed.ShowRule
	Roots        Roots
	AnthropicKey string // empty = no AI fallback
	Mode         Mode   // "" defaults to ModeEnqueue
}

// effectiveMode resolves an empty Mode to the default.
func (c Config) effectiveMode() Mode {
	if c.Mode == "" {
		return ModeEnqueue
	}
	return c.Mode
}

// extractTitle applies a feed's optional title-shaping regex. If re is nil, or
// the regex misses, the raw title is returned unchanged.
func extractTitle(re *regexp.Regexp, raw string) string {
	if re == nil {
		return raw
	}
	m := re.FindStringSubmatch(raw)
	if len(m) < 2 {
		return raw
	}
	return m[1]
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

	mode := cfg.effectiveMode()
	slog.Info("run_start", "mode", string(mode), "feeds", r.Feeds, "shows", len(cfg.Shows))

	// startup jitter — skipped if context already has a deadline shorter than 30s
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > 30*time.Second {
		jitter := time.Duration(rand.Intn(30_000)) * time.Millisecond
		slog.Info("jitter_sleep", "ms", jitter.Milliseconds())
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(jitter):
		}
	}

	for _, f := range cfg.Feeds {
		feedLabel := f.Name
		if feedLabel == "" {
			feedLabel = f.URL
		}
		slog.Info("feed_fetch_start", "feed", feedLabel, "cookie_present", f.Cookie != "")
		fetchStart := time.Now()
		items, err := feed.Fetch(ctx, f.URL, f.Cookie)
		if err != nil {
			slog.Error("feed_fetch_error",
				"feed", feedLabel,
				"duration_ms", time.Since(fetchStart).Milliseconds(),
				"err", err)
			r.Errors++
			continue
		}
		slog.Info("feed_fetch_done",
			"feed", feedLabel,
			"items", len(items),
			"duration_ms", time.Since(fetchStart).Milliseconds())
		r.Items += len(items)
		for _, it := range items {
			it.Title = extractTitle(f.TitleExtract, it.Title)
			rule, ok := feed.Match(it.Title, feedLabel, cfg.Shows)
			if !ok {
				slog.Debug("item_no_match", "feed", feedLabel, "title", it.Title)
				continue
			}
			r.Matched++
			hashFromFeed, _ := feed.ExtractHash(it)
			slog.Info("item_matched",
				"feed", feedLabel, "title", it.Title,
				"rule", rule.Name, "hash_from_feed", hashFromFeed)
			if err := processItem(ctx, cfg, hc, f.Cookie, it, rule, &r); err != nil {
				slog.Error("process_item", "title", it.Title, "err", err)
				r.Errors++
			}
		}
	}
	slog.Info("run_summary",
		"mode", string(mode),
		"feeds", r.Feeds, "items", r.Items, "matched", r.Matched,
		"queried", r.Queried, "added", r.Added, "errors", r.Errors)
	return r, nil
}

func processItem(ctx context.Context, cfg Config, hc *http.Client, feedCookie string, it feed.Item, rule feed.ShowRule, r *RunResult) error {
	mode := cfg.effectiveMode()

	hash, hashFromFeed := feed.ExtractHash(it)
	var torrentBytes []byte

	// observe mode: log everything we can determine without fetching, then return.
	if mode == ModeObserve {
		slog.Info("observe_skip_fetch",
			"title", it.Title, "rule", rule.Name,
			"hash", hash, "hash_from_feed", hashFromFeed,
			"enclosure", it.Enclosure)
		return nil
	}

	if !hashFromFeed {
		var err error
		torrentBytes, err = fetchBytes(ctx, hc, it.Enclosure, feedCookie)
		if err != nil {
			return fmt.Errorf("fetch torrent: %w", err)
		}
		hash, err = feed.ComputeHashFromTorrentBytes(torrentBytes)
		if err != nil {
			return fmt.Errorf("compute hash: %w", err)
		}
		slog.Info("torrent_fetched_and_hashed", "title", it.Title, "hash", hash, "bytes", len(torrentBytes))
	}

	// compute save directory based on category
	saveDir := ""
	switch rule.Category {
	case "tv":
		if cfg.Roots.TV == "" {
			slog.Warn("tv_save_dir_skipped_no_root", "title", it.Title, "rule", rule.Name)
			break
		}
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
	slog.Info("dedup_check", "title", it.Title, "hash", hash, "exists_on_seedctl", exists)
	if exists {
		slog.Info("skip_already_present", "title", it.Title, "hash", hash, "rule", rule.Name)
		return nil
	}

	// fetch mode: stop short of POSTing.
	if mode == ModeFetch {
		slog.Info("fetch_mode_skip_enqueue",
			"title", it.Title, "hash", hash, "rule", rule.Name, "save_dir", saveDir)
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
