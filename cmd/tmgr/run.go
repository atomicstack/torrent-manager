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
	"time"

	"github.com/atomicstack/torrent-manager/internal/feed"
)

// Config holds the runtime parameters for a single Run tick.
type Config struct {
	SeedctlURL    string
	TrackerCookie string
	Feeds         []string
	Shows         []feed.ShowRule
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

	for _, url := range cfg.Feeds {
		items, err := feed.Fetch(ctx, url, cfg.TrackerCookie)
		if err != nil {
			slog.Error("feed_fetch", "feed", url, "err", err)
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
			if err := processItem(ctx, cfg, hc, it, rule, &r); err != nil {
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

func processItem(ctx context.Context, cfg Config, hc *http.Client, it feed.Item, rule feed.ShowRule, r *RunResult) error {
	hash, ok := feed.ExtractHash(it)
	var torrentBytes []byte
	if !ok {
		var err error
		torrentBytes, err = fetchBytes(ctx, hc, it.Enclosure, cfg.TrackerCookie)
		if err != nil {
			return fmt.Errorf("fetch torrent: %w", err)
		}
		hash, err = feed.ComputeHashFromTorrentBytes(torrentBytes)
		if err != nil {
			return fmt.Errorf("compute hash: %w", err)
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
		torrentBytes, err = fetchBytes(ctx, hc, it.Enclosure, cfg.TrackerCookie)
		if err != nil {
			return fmt.Errorf("fetch torrent: %w", err)
		}
	}
	if err := seedctlAdd(ctx, hc, cfg.SeedctlURL, hash, torrentBytes); err != nil {
		return fmt.Errorf("seedctl add: %w", err)
	}
	r.Added++
	slog.Info("added", "title", it.Title, "hash", hash, "rule", rule.Name)
	return nil
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

func seedctlAdd(ctx context.Context, hc *http.Client, base, hash string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/torrents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", hash)
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
