// Package main runs the tmweb http server: a lan-only web ui for browsing the
// rss items index in seedctl and (in later tasks) triggering manual enqueues.
//
// As of task 13 this binary serves:
//   - GET /healthz       — liveness + seedctl ping (json)
//   - GET /              — html shell with sticky search bar + feed dropdown
//   - GET /api/search    — htmx-driven table-rows fragment for the search UI
//   - GET /static/*      — embedded css + vendored htmx.min.js
//
// Task 14 will add a POST /api/enqueue endpoint for manual enqueues; until
// then the per-row "enqueue" buttons render disabled.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/atomicstack/torrent-manager/internal/watchlist"
)

func main() {
	listenAddr := flag.String("listen", ":8080", "http listen address (lan-only, no tls)")
	seedctlURL := flag.String("seedctl-url", "https://seedbox.example.com:18443", "seedctl base url")
	watchPath := flag.String("watchlist", "/etc/tmgr/watchlist.yaml", "watchlist config")
	caPath := flag.String("ca", "/etc/tmgr/tls/ca.crt", "ca cert (mtls to seedctl)")
	certPath := flag.String("cert", "/etc/tmgr/tls/client.crt", "client cert (mtls)")
	keyPath := flag.String("key", "/etc/tmgr/tls/client.key", "client key (mtls)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// load + compile the watchlist up-front so a malformed config fails fast
	// rather than waiting for the first request that needs it. tmweb itself
	// uses File.Feeds for the dropdown options; task 14 will use the compiled
	// shows to scope rule-matched enqueues.
	file, err := watchlist.Load(*watchPath)
	if err != nil {
		logger.Error("load watchlist", "err", err)
		os.Exit(1)
	}
	if _, _, err := watchlist.Compile(file); err != nil {
		logger.Error("compile watchlist", "err", err)
		os.Exit(1)
	}

	feeds := feedNames(file)

	tlsCfg, err := tlsx.ClientConfig(*caPath, *certPath, *keyPath)
	if err != nil {
		logger.Error("tls", "err", err)
		os.Exit(1)
	}
	httpC := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	seedctl := NewSeedctl(*seedctlURL, httpC)

	r, err := newRenderer()
	if err != nil {
		logger.Error("renderer", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           newMux(seedctl, feeds, r),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("listening", "addr", *listenAddr, "seedctl_url", *seedctlURL, "feeds", len(feeds))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting_down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// feedNames returns the names of all configured feeds, preserving watchlist
// order. Feeds without a name are skipped — the dropdown shows what the
// items rows actually carry as feed_name, which requires a configured name.
func feedNames(f *watchlist.File) []string {
	out := make([]string, 0, len(f.Feeds))
	for _, ff := range f.Feeds {
		if ff.Name != "" {
			out = append(out, ff.Name)
		}
	}
	return out
}

// handler bundles the dependencies the http endpoints need. Bundling them
// into a struct keeps newMux signature stable as endpoints are added and
// makes the index/search handlers trivially testable with stubbed seedctl.
type handler struct {
	seedctl *Seedctl
	feeds   []string
	r       *renderer
}

// newMux wires the http routes for tmweb. Extracted from main so tests can
// instantiate a handler against a stubbed *Seedctl with a known feed list.
func newMux(s *Seedctl, feeds []string, r *renderer) http.Handler {
	h := &handler{seedctl: s, feeds: feeds, r: r}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		// 5s timeout so a wedged seedctl can't hang a k8s liveness probe.
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		ok, err := s.Healthz(ctx)
		if err != nil {
			slog.Debug("healthz_seedctl_ping_failed", "err", err)
		}
		// always 200: kubernetes liveness probes care about the body, and we
		// want the probe to read the json so it can distinguish "tmweb up,
		// seedctl down" from "tmweb hung".
		writeJSON(w, http.StatusOK, healthzResponse{
			OK:        ok,
			SeedctlOK: ok,
		})
	})
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("GET /api/search", h.search)
	mux.Handle("GET /static/", staticHandler())
	return mux
}

// index renders the full page shell. The table body starts empty ("loading…")
// and is populated by htmx via /api/search on page load (hx-trigger="load").
func (h *handler) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.r.layout.Execute(w, struct{ Feeds []string }{Feeds: h.feeds}); err != nil {
		// Best-effort: the response writer may have already flushed status
		// 200 + partial body, so http.Error may no-op. That's fine — htmx
		// will treat truncated html as a swap failure and leave the row
		// placeholder visible.
		slog.Error("render_layout", "err", err)
		http.Error(w, "render", http.StatusInternalServerError)
	}
}

// search renders just the <tbody> rows for the htmx swap. Errors from
// seedctl are rendered as a single error row so htmx still gets a coherent
// fragment to swap in (instead of leaving "loading…" stuck on the page).
func (h *handler) search(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	items, err := h.seedctl.ListItems(req.Context(), ItemsQuery{
		Q:               q.Get("q"),
		Feed:            q.Get("feed"),
		IncludeEnqueued: q.Get("include_enqueued") == "1",
		Limit:           200,
	})
	if err != nil {
		slog.Debug("search_seedctl_failed", "err", err)
		// htmx-friendly: return 200 with an error row so the table body
		// shows a useful message instead of an empty/loading state. The
		// error message comes from the seedctl client which already
		// scrubs sensitive info (just status + path).
		w.WriteHeader(http.StatusOK)
		// escape the error message to avoid breaking the surrounding <tr>
		// if seedctl returned a URL containing < or & in the error string.
		fmt.Fprintf(w, `<tr><td colspan="5" class="empty">error: %s</td></tr>`, html.EscapeString(err.Error()))
		return
	}
	if err := h.r.rows.Execute(w, items); err != nil {
		slog.Error("render_rows", "err", err)
		http.Error(w, "render", http.StatusInternalServerError)
	}
}

type healthzResponse struct {
	OK        bool `json:"ok"`
	SeedctlOK bool `json:"seedctl_ok"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
