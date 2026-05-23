package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/atomicstack/torrent-manager/internal/hashcache"
	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/atomicstack/torrent-manager/internal/watchlist"
	// sqlite3 driver registration lives at the binary root rather than inside
	// internal/hashcache so the side effect is visible from main.
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	watchPath := flag.String("watchlist", "/etc/tmgr/watchlist.yaml", "watchlist config")
	caPath := flag.String("ca", "/etc/tmgr/tls/ca.crt", "ca cert")
	certPath := flag.String("cert", "/etc/tmgr/tls/client.crt", "client cert")
	keyPath := flag.String("key", "/etc/tmgr/tls/client.key", "client key")
	modeFlag := flag.String("mode", "enqueue", "pipeline mode: observe (log only), fetch (download+dedup, no POST), enqueue (full)")
	cachePath := flag.String("cache-file", "", "path to a sqlite db caching positive seedctl dedup hits; empty = no cache. a sibling .json file (legacy format) is imported once on startup then renamed to .json.imported")
	flag.Parse()

	var mode Mode
	switch *modeFlag {
	case "observe":
		mode = ModeObserve
	case "fetch":
		mode = ModeFetch
	case "enqueue":
		mode = ModeEnqueue
	default:
		fmt.Fprintf(os.Stderr, "invalid --mode=%q (want observe|fetch|enqueue)\n", *modeFlag)
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	file, err := watchlist.Load(*watchPath)
	if err != nil {
		logger.Error("load watchlist", "err", err)
		os.Exit(1)
	}
	shows, titleExtracts, err := watchlist.Compile(file)
	if err != nil {
		logger.Error("compile watchlist", "err", err)
		os.Exit(1)
	}

	// Build runtime Feed structs. Cookie env lookup + per-env-dedup logging is
	// a tmgr-runtime concern (touches os.Getenv and the tmgr logger), so it
	// stays here rather than inside the watchlist package.
	loggedCookieEnv := make(map[string]struct{}, len(file.Feeds))
	feeds := make([]Feed, 0, len(file.Feeds))
	for i, ff := range file.Feeds {
		var cookie string
		if ff.CookieEnv != "" {
			cookie = os.Getenv(ff.CookieEnv)
			if _, seen := loggedCookieEnv[ff.CookieEnv]; !seen {
				loggedCookieEnv[ff.CookieEnv] = struct{}{}
				if cookie == "" {
					logger.Warn("cookie_env not set; feeds bound to it will poll without a cookie",
						"env", ff.CookieEnv)
				} else {
					l := len(cookie)
					if l < 24 {
						logger.Info("tracker_cookie", "env", ff.CookieEnv, "len", l)
					} else {
						logger.Info("tracker_cookie", "env", ff.CookieEnv, "len", l,
							"head8", cookie[:8], "tail8", cookie[l-8:])
					}
				}
			}
		}
		feeds = append(feeds, Feed{
			Name:         ff.Name,
			URL:          ff.URL,
			Cookie:       cookie,
			Category:     ff.Category,
			TitleExtract: titleExtracts[i],
		})
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if anthropicKey == "" {
		logger.Warn("ANTHROPIC_API_KEY not set; ai title fallback disabled")
	}

	tlsCfg, err := tlsx.ClientConfig(*caPath, *certPath, *keyPath)
	if err != nil {
		logger.Error("tls", "err", err)
		os.Exit(1)
	}
	httpC := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}

	var cache *hashcache.Cache
	if *cachePath != "" {
		c, cerr := hashcache.New(*cachePath)
		if cerr != nil {
			logger.Warn("hashcache open failed; running without cache", "path", *cachePath, "err", cerr)
		} else {
			cache = c
			logger.Info("hashcache_loaded",
				"path", *cachePath,
				"hash_count", c.Len(),
				"file_size", c.FileSize(),
				"initialised", c.Initialised())

			// one-shot migration: if the cache is empty and a sibling legacy
			// json file exists, import it then rename to .json.imported so we
			// don't re-import on subsequent runs.
			if !c.Initialised() {
				jsonPath := strings.TrimSuffix(*cachePath, ".db") + ".json"
				if jsonPath != *cachePath {
					if entries := importLegacyJSON(jsonPath); len(entries) > 0 {
						if err := c.Populate(entries); err == nil {
							_ = os.Rename(jsonPath, jsonPath+".imported")
							logger.Info("hashcache_imported_from_json",
								"from", jsonPath, "to", *cachePath, "count", len(entries))
						} else {
							logger.Warn("hashcache_import_failed", "err", err)
						}
					}
				}
			}
		}
	}

	cfg := Config{
		SeedctlURL: file.SeedctlURL,
		Feeds:      feeds,
		Shows:      shows,
		Roots: Roots{
			TV:     file.Roots.TV,
			Movies: file.Roots.Movies,
		},
		AnthropicKey: anthropicKey,
		Mode:         mode,
		Cache:        cache,
	}

	if _, err := Run(context.Background(), cfg, httpC); err != nil {
		logger.Error("run", "err", err)
		os.Exit(1)
	}
}

// importLegacyJSON reads the legacy {"torrents": [...]} JSON cache file. It
// returns nil if the file is missing or malformed — the caller treats that as
// "nothing to import" without error.
func importLegacyJSON(jsonPath string) []hashcache.Entry {
	body, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Torrents []hashcache.Entry `json:"torrents"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	return doc.Torrents
}
