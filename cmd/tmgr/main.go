package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"regexp"

	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"gopkg.in/yaml.v3"
)

type fileFeed struct {
	URL       string `yaml:"url"`
	CookieEnv string `yaml:"cookie_env,omitempty"`
}

type fileConfig struct {
	SeedctlURL string `yaml:"seedctl_url"`
	Shows      []struct {
		Name  string `yaml:"name"`
		Regex string `yaml:"regex"`
	} `yaml:"shows"`
	Feeds []fileFeed `yaml:"feeds"`
}

func main() {
	watchPath := flag.String("watchlist", "/etc/tmgr/watchlist.yaml", "watchlist config")
	caPath := flag.String("ca", "/etc/tmgr/tls/ca.crt", "ca cert")
	certPath := flag.String("cert", "/etc/tmgr/tls/client.crt", "client cert")
	keyPath := flag.String("key", "/etc/tmgr/tls/client.key", "client key")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	b, err := os.ReadFile(*watchPath)
	if err != nil {
		logger.Error("read watchlist", "err", err)
		os.Exit(1)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(b, &fc); err != nil {
		logger.Error("parse watchlist", "err", err)
		os.Exit(1)
	}

	shows := make([]feed.ShowRule, 0, len(fc.Shows))
	for _, s := range fc.Shows {
		re, err := regexp.Compile(s.Regex)
		if err != nil {
			logger.Error("bad regex", "name", s.Name, "err", err)
			os.Exit(1)
		}
		shows = append(shows, feed.ShowRule{Name: s.Name, Regex: re})
	}

	feeds := make([]Feed, 0, len(fc.Feeds))
	for _, ff := range fc.Feeds {
		var cookie string
		if ff.CookieEnv != "" {
			cookie = os.Getenv(ff.CookieEnv)
			if cookie == "" {
				logger.Warn("cookie_env not set; feed will be polled without a cookie",
					"env", ff.CookieEnv, "feed", ff.URL)
			} else {
				l := len(cookie)
				if l < 16 {
					logger.Info("tracker_cookie", "env", ff.CookieEnv, "len", l)
				} else {
					logger.Info("tracker_cookie", "env", ff.CookieEnv, "len", l,
						"head8", cookie[:8], "tail8", cookie[l-8:])
				}
			}
		}
		feeds = append(feeds, Feed{URL: ff.URL, Cookie: cookie})
	}

	tlsCfg, err := tlsx.ClientConfig(*caPath, *certPath, *keyPath)
	if err != nil {
		logger.Error("tls", "err", err)
		os.Exit(1)
	}
	httpC := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}

	cfg := Config{
		SeedctlURL: fc.SeedctlURL,
		Feeds:      feeds,
		Shows:      shows,
	}

	if _, err := Run(context.Background(), cfg, httpC); err != nil {
		logger.Error("run", "err", err)
		os.Exit(1)
	}
}
