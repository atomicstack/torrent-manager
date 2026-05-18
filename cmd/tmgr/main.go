package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"

	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"gopkg.in/yaml.v3"
)

type fileFeed struct {
	Name      string `yaml:"name,omitempty"`
	URL       string `yaml:"url"`
	CookieEnv string `yaml:"cookie_env,omitempty"`
}

type fileRoots struct {
	TV     string `yaml:"tv"`
	Movies string `yaml:"movies"`
}

type fileConfig struct {
	SeedctlURL string    `yaml:"seedctl_url"`
	Roots      fileRoots `yaml:"roots"`
	Shows      []struct {
		Name      string   `yaml:"name"`
		Regex     string   `yaml:"regex"`
		Category  string   `yaml:"category,omitempty"`
		Directory string   `yaml:"directory,omitempty"`
		Feeds     []string `yaml:"feeds,omitempty"`
	} `yaml:"shows"`
	Feeds []fileFeed `yaml:"feeds"`
}

func main() {
	watchPath := flag.String("watchlist", "/etc/tmgr/watchlist.yaml", "watchlist config")
	caPath := flag.String("ca", "/etc/tmgr/tls/ca.crt", "ca cert")
	certPath := flag.String("cert", "/etc/tmgr/tls/client.crt", "client cert")
	keyPath := flag.String("key", "/etc/tmgr/tls/client.key", "client key")
	modeFlag := flag.String("mode", "enqueue", "pipeline mode: observe (log only), fetch (download+dedup, no POST), enqueue (full)")
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

	feedNames := make(map[string]struct{}, len(fc.Feeds))
	for _, ff := range fc.Feeds {
		if ff.Name != "" {
			feedNames[ff.Name] = struct{}{}
		}
	}

	shows := make([]feed.ShowRule, 0, len(fc.Shows))
	for _, s := range fc.Shows {
		re, err := regexp.Compile(s.Regex)
		if err != nil {
			logger.Error("bad regex", "name", s.Name, "err", err)
			os.Exit(1)
		}
		for _, fn := range s.Feeds {
			if _, ok := feedNames[fn]; !ok {
				logger.Error("show rule references unknown feed",
					"rule", s.Name, "feed", fn)
				os.Exit(1)
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
				if l < 24 {
					logger.Info("tracker_cookie", "env", ff.CookieEnv, "len", l)
				} else {
					logger.Info("tracker_cookie", "env", ff.CookieEnv, "len", l,
						"head8", cookie[:8], "tail8", cookie[l-8:])
				}
			}
		}
		feeds = append(feeds, Feed{Name: ff.Name, URL: ff.URL, Cookie: cookie})
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

	cfg := Config{
		SeedctlURL: fc.SeedctlURL,
		Feeds:      feeds,
		Shows:      shows,
		Roots: Roots{
			TV:     fc.Roots.TV,
			Movies: fc.Roots.Movies,
		},
		AnthropicKey: anthropicKey,
		Mode:         mode,
	}

	if _, err := Run(context.Background(), cfg, httpC); err != nil {
		logger.Error("run", "err", err)
		os.Exit(1)
	}
}
