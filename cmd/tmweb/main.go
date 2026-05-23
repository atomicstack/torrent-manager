// Package main runs the tmweb http server: a lan-only web ui for browsing the
// rss items index in seedctl and (in later tasks) triggering manual enqueues.
//
// As of task 14 this binary serves:
//   - GET  /healthz             — liveness + seedctl ping (json)
//   - GET  /                    — html shell with sticky search bar + feed dropdown
//   - GET  /api/search          — htmx-driven table-rows fragment for the search UI
//   - GET  /api/enqueue-confirm — modal fragment confirming a rule-matched enqueue
//   - POST /api/enqueue         — fetches the .torrent, posts it to seedctl, returns refreshed rows
//   - GET  /static/*            — embedded css + vendored htmx.min.js
//
// Phase 3 will wire a Claude-driven flow for items without a matching rule;
// today those rows render a disabled button instead.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/savedir"
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
	// uses File.Feeds for the dropdown options + cookie env mapping, and the
	// compiled shows to scope rule-matched enqueues.
	file, err := watchlist.Load(*watchPath)
	if err != nil {
		logger.Error("load watchlist", "err", err)
		os.Exit(1)
	}
	shows, _, err := watchlist.Compile(file)
	if err != nil {
		logger.Error("compile watchlist", "err", err)
		os.Exit(1)
	}

	feeds := feedNames(file)
	roots := savedir.Roots{TV: file.Roots.TV, Movies: file.Roots.Movies}
	cookieEnvs := cookieEnvMap(file)

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
		Handler:           newMux(seedctl, feeds, shows, roots, cookieEnvs, r),
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

// cookieEnvMap builds a feed-name -> cookie env var name lookup. Feeds with
// no cookie_env declared are omitted (callers treat absence as "no cookie").
func cookieEnvMap(f *watchlist.File) map[string]string {
	out := make(map[string]string, len(f.Feeds))
	for _, ff := range f.Feeds {
		if ff.Name != "" && ff.CookieEnv != "" {
			out[ff.Name] = ff.CookieEnv
		}
	}
	return out
}

// handler bundles the dependencies the http endpoints need. Bundling them
// into a struct keeps newMux signature stable as endpoints are added and
// makes the handlers trivially testable with stubbed seedctl + in-memory
// watchlist fixtures.
type handler struct {
	seedctl    *Seedctl
	feeds      []string          // for the dropdown
	shows      []feed.ShowRule   // for rule matching at enqueue time
	roots      savedir.Roots     // for savedir.Derive
	cookieEnvs map[string]string // feedName -> CookieEnv (e.g. "TrackerA (TV)" -> "COOKIE_TRACKER_A")
	r          *renderer
}

// newMux wires the http routes for tmweb. Extracted from main so tests can
// instantiate a handler against a stubbed *Seedctl with a known feed list.
func newMux(s *Seedctl, feeds []string, shows []feed.ShowRule, roots savedir.Roots, cookieEnvs map[string]string, r *renderer) http.Handler {
	h := &handler{seedctl: s, feeds: feeds, shows: shows, roots: roots, cookieEnvs: cookieEnvs, r: r}

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
	mux.HandleFunc("GET /api/enqueue-confirm", h.enqueueConfirm)
	mux.HandleFunc("POST /api/enqueue", h.enqueueDo)
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

// enqueueConfirm renders the modal fragment for a rule-matched enqueue.
// Looks up the item by (feed, guid), re-derives the save_dir, and pushes
// it through the _confirm.html template. Errors render as a tiny modal
// card with an error message so the user always sees something on click.
func (h *handler) enqueueConfirm(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	q := req.URL.Query()
	it, err := h.lookupItem(req.Context(), q.Get("feed"), q.Get("guid"))
	if err != nil {
		renderModalError(w, "lookup failed", err.Error())
		return
	}
	if it.MatchedRule == "" {
		// phase 3 will replace this with the Claude-driven manual flow.
		renderModalError(w, "no rule",
			"this item has no matching watchlist rule. phase 3 will support manual enqueue.")
		return
	}
	rule := h.findRule(it.MatchedRule)
	if rule == nil {
		renderModalError(w, "rule missing",
			fmt.Sprintf("watchlist no longer has rule %q. reload the page?", it.MatchedRule))
		return
	}
	saveDir, _ := savedir.Derive(h.roots, *rule, it.Title)
	if err := h.r.confirm.Execute(w, struct {
		Item    api.ItemRecord
		SaveDir string
	}{Item: it, SaveDir: saveDir}); err != nil {
		slog.Error("render_confirm", "err", err)
	}
}

// enqueueDo executes the enqueue: fetch the .torrent, compute the hash if
// missing, POST to seedctl, then re-render the table body so the row picks
// up its new "auto-enqueued" badge. On failure we return an error row so
// the user sees the cause inline instead of a silent no-op.
func (h *handler) enqueueDo(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	q := req.URL.Query()
	it, err := h.lookupItem(req.Context(), q.Get("feed"), q.Get("guid"))
	if err != nil {
		h.renderEnqueueError(w, fmt.Sprintf("lookup: %s", err))
		return
	}
	if it.MatchedRule == "" {
		h.renderEnqueueError(w, "no matching watchlist rule (phase 3 will support manual enqueue)")
		return
	}
	rule := h.findRule(it.MatchedRule)
	if rule == nil {
		h.renderEnqueueError(w, fmt.Sprintf("watchlist rule %q no longer present", it.MatchedRule))
		return
	}
	saveDir, _ := savedir.Derive(h.roots, *rule, it.Title)

	// fetchTorrent uses http.DefaultClient because tracker URLs are served
	// off public CA, NOT mTLS. The cookie comes from the per-feed env var
	// declared in the watchlist (e.g. COOKIE_TRACKER_A for TrackerA feeds); task 15
	// mounts the same tmgr-cookies secret into tmweb's deployment so this
	// works in production.
	cookie := os.Getenv(h.cookieEnvFor(it.FeedName))
	body, err := fetchTorrent(req.Context(), it.Link, cookie)
	if err != nil {
		h.renderEnqueueError(w, fmt.Sprintf("fetch %s: %s", it.Link, err))
		return
	}
	hash := it.InfoHash
	if hash == "" {
		h2, herr := feed.ComputeHashFromTorrentBytes(body)
		if herr != nil {
			h.renderEnqueueError(w, fmt.Sprintf("compute hash: %s", herr))
			return
		}
		hash = h2
	}
	if _, err := h.seedctl.PostTorrent(req.Context(), hash, body, saveDir); err != nil {
		h.renderEnqueueError(w, fmt.Sprintf("seedctl add: %s", err))
		return
	}
	slog.Info("manual_enqueue_done",
		"feed", it.FeedName, "guid", it.GUID, "hash", hash, "save_dir", saveDir)

	// re-render the whole table so the row picks up its new badge. The
	// source written by seedctl is "auto" — phase 3 may switch this to
	// "manual" but the badge is already informative as-is.
	h.search(w, req)
}

// lookupItem fetches an item by (feed, guid) by listing the feed with
// include_enqueued=1 and scanning. Not efficient but adequate at the
// expected ≤200 items/feed; phase 3 could add a dedicated
// /v1/items/by-id endpoint to seedctl if this becomes a hot path.
//
// TODO(phase3): add seedctl GET /v1/items/by-id?feed=&guid= to avoid the
// full-feed scan.
func (h *handler) lookupItem(ctx context.Context, feed, guid string) (api.ItemRecord, error) {
	if feed == "" || guid == "" {
		return api.ItemRecord{}, fmt.Errorf("missing feed or guid")
	}
	resp, err := h.seedctl.ListItems(ctx, ItemsQuery{
		Feed:            feed,
		IncludeEnqueued: true,
		Limit:           1000,
	})
	if err != nil {
		return api.ItemRecord{}, err
	}
	for _, it := range resp.Items {
		if it.GUID == guid {
			return it, nil
		}
	}
	return api.ItemRecord{}, fmt.Errorf("item not found: feed=%q guid=%q", feed, guid)
}

// findRule returns the compiled ShowRule with the given name, or nil if
// the watchlist has been reloaded and the rule no longer exists.
func (h *handler) findRule(name string) *feed.ShowRule {
	for i := range h.shows {
		if h.shows[i].Name == name {
			return &h.shows[i]
		}
	}
	return nil
}

// cookieEnvFor returns the env var name to look up the cookie under for
// the given feed, or "" if the feed has no cookie_env declared.
func (h *handler) cookieEnvFor(feedName string) string {
	return h.cookieEnvs[feedName]
}

// renderEnqueueError writes a single error row for the rows tbody swap.
// We always return 200 so htmx applies the swap (4xx would skip it by
// default) — the user sees the error inline in the table.
func (h *handler) renderEnqueueError(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<tr><td colspan="5" class="empty">enqueue failed: %s</td></tr>`,
		html.EscapeString(msg))
}

// renderModalError writes a minimal modal card with an error message.
// Used by enqueueConfirm before a successful confirm render takes over.
func renderModalError(w http.ResponseWriter, heading, msg string) {
	fmt.Fprintf(w,
		`<div class="modal-bg" hx-on:click="document.getElementById('modal').innerHTML=''">`+
			`<div class="modal-card" onclick="event.stopPropagation()">`+
			`<h2>%s</h2><p>%s</p>`+
			`<div class="modal-actions">`+
			`<button class="btn-cancel" hx-on:click="document.getElementById('modal').innerHTML=''">close</button>`+
			`</div></div></div>`,
		html.EscapeString(heading), html.EscapeString(msg))
}

// fetchTorrent downloads a .torrent file URL. Uses http.DefaultClient since
// tracker URLs are served via public CA, NOT mTLS. The cookie (if any)
// comes from the per-feed cookie_env declared in the watchlist; tmweb
// reads it via os.Getenv in enqueueDo. Task 15 mounts the same
// tmgr-cookies secret used by tmgr into the tmweb deployment.
func fetchTorrent(ctx context.Context, url, cookie string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// drain to allow connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
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
