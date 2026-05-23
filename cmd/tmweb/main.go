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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/aiclassify"
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
	feedCats := feedCategoryMap(file)

	// aiclassify is optional: without ANTHROPIC_API_KEY the no-rule tv path
	// renders the modal with an empty save_dir + a note that auto-classify
	// failed. tmweb still works for rule-matched + movies + other paths.
	var ai *aiclassify.Client
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		ai = aiclassify.New(key)
	}

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
		Handler:           newMux(seedctl, feeds, cookieEnvs, feedCats, shows, roots, ai, r),
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

// feedCategoryMap builds a feed-name -> category lookup ("tv" | "movies" |
// "other"). Used by the no-rule enqueue flow to decide how to propose a
// save_dir. Feeds with no category declared are omitted (the handler treats
// absence as "other" — user enters the dir manually).
func feedCategoryMap(f *watchlist.File) map[string]string {
	out := make(map[string]string, len(f.Feeds))
	for _, ff := range f.Feeds {
		if ff.Name != "" && ff.Category != "" {
			out[ff.Name] = ff.Category
		}
	}
	return out
}

// handler bundles the dependencies the http endpoints need. Bundling them
// into a struct keeps newMux signature stable as endpoints are added and
// makes the handlers trivially testable with stubbed seedctl + in-memory
// watchlist fixtures.
type handler struct {
	seedctl        *Seedctl
	feeds          []string           // for the dropdown
	shows          []feed.ShowRule    // for rule matching at enqueue time
	roots          savedir.Roots      // for savedir.Derive
	cookieEnvs     map[string]string  // feedName -> CookieEnv (e.g. "TrackerA (TV)" -> "COOKIE_TRACKER_A")
	feedCategories map[string]string  // feedName -> "tv"|"movies"|"other" (no-rule flow)
	ai             *aiclassify.Client // nil if ANTHROPIC_API_KEY unset; no-rule tv path falls back to manual entry
	r              *renderer
}

// newMux wires the http routes for tmweb. Extracted from main so tests can
// instantiate a handler against a stubbed *Seedctl with a known feed list.
func newMux(
	s *Seedctl,
	feeds []string,
	cookieEnvs map[string]string,
	feedCategories map[string]string,
	shows []feed.ShowRule,
	roots savedir.Roots,
	ai *aiclassify.Client,
	r *renderer,
) http.Handler {
	h := &handler{
		seedctl:        s,
		feeds:          feeds,
		shows:          shows,
		roots:          roots,
		cookieEnvs:     cookieEnvs,
		feedCategories: feedCategories,
		ai:             ai,
		r:              r,
	}

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
	mux.HandleFunc("GET /api/enqueue-classify", h.enqueueClassify)
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
	h.renderRows(w, req.Context(), ItemsQuery{
		Q:               q.Get("q"),
		Feed:            q.Get("feed_filter"),
		IncludeEnqueued: q.Get("include_enqueued") == "1",
		Limit:           200,
	})
}

// renderRows queries seedctl and writes the table-rows html fragment, or an
// error row on failure. Shared by GET /api/search and POST /api/enqueue
// (which re-renders with the user's preserved filter state after a
// successful enqueue).
func (h *handler) renderRows(w http.ResponseWriter, ctx context.Context, q ItemsQuery) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	items, err := h.seedctl.ListItems(ctx, q)
	if err != nil {
		slog.Debug("search_seedctl_failed", "err", err)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `<tr><td colspan="4" class="empty">error: %s</td></tr>`, html.EscapeString(err.Error()))
		return
	}
	if err := h.r.rows.Execute(w, items); err != nil {
		slog.Error("render_rows", "err", err)
		http.Error(w, "render", http.StatusInternalServerError)
	}
}

// enqueueConfirm renders the modal fragment for the enqueue flow. Branches
// on whether the item has a matched watchlist rule:
//   - rule-matched   → _confirm.html, save_dir pre-derived from the rule.
//   - no rule, movies → _confirm_norule.html with roots.Movies pre-filled.
//   - no rule, tv     → ask seedctl for the existing tv dirs, then claude for
//     a best-fit show dir + season. render _confirm_norule.html with the
//     proposed path; if any step fails, leave the field empty + flag
//     auto-pick failure so the user knows to fill it in by hand.
//   - no rule, other  → _confirm_norule.html with an empty save_dir; the
//     user can type one or leave blank for rtorrent's default.
func (h *handler) enqueueConfirm(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	q := req.URL.Query()
	it, err := h.lookupItem(req.Context(), q.Get("feed"), q.Get("guid"))
	if err != nil {
		renderModalError(w, "lookup failed", err.Error())
		return
	}
	if it.MatchedRule != "" {
		h.confirmRuleMatched(w, it)
		return
	}
	h.confirmNoRule(w, req.Context(), it)
}

// confirmRuleMatched is the original rule-matched path: re-derive the
// save_dir from the watchlist rule and render the _confirm.html modal.
func (h *handler) confirmRuleMatched(w http.ResponseWriter, it api.ItemRecord) {
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

// confirmNoRule renders the _confirm_norule.html modal with a category-
// specific proposed save_dir. Any failure to compute the proposal (ai
// disabled, seedctl down, claude error, missing roots) renders the same
// modal with an empty field + AutoPickFailed=true so the user always has
// somewhere to type the path.
// confirmNoRuleData is the data the no-rule modal template receives. The
// LoadingURL field is non-empty only for the tv path where we want the modal
// to open instantly and let htmx fire the slow claude lookup in the background.
// For movies / other / ai-disabled paths the form is rendered inline and
// LoadingURL stays empty.
type confirmNoRuleData struct {
	Item            api.ItemRecord
	Category        string
	ProposedSaveDir string
	ClaudePicked    bool
	AutoPickFailed  bool
	LoadingURL      string
}

func (h *handler) confirmNoRule(w http.ResponseWriter, ctx context.Context, it api.ItemRecord) {
	category := h.feedCategories[it.FeedName]
	data := confirmNoRuleData{Item: it, Category: category}

	switch category {
	case "movies":
		// movies just live under roots.Movies — no subdir convention. Render
		// the form inline; no claude call needed.
		data.ProposedSaveDir = h.roots.Movies
	case "tv":
		if h.ai == nil {
			// ANTHROPIC_API_KEY missing: skip the claude call and let the
			// user enter the dir manually. Render the form inline.
			data.AutoPickFailed = true
			break
		}
		// Tv path: return the modal shell with a loading state immediately.
		// htmx fires GET /api/enqueue-classify on load; that endpoint does
		// the slow seedctl + claude calls and returns the form fragment
		// to outerHTML-swap the loading placeholder.
		data.LoadingURL = fmt.Sprintf("/api/enqueue-classify?feed=%s&guid=%s",
			url.QueryEscape(it.FeedName), url.QueryEscape(it.GUID))
	default:
		// "other" / unknown: render the form inline with an empty path.
	}
	if err := h.r.confirmNoRule.Execute(w, data); err != nil {
		slog.Error("render_confirm_norule", "err", err)
	}
}

// enqueueClassify performs the slow path of the no-rule tv flow: ask seedctl
// for the existing show dirs, ask claude to pick the best fit + extract the
// season, and return just the inner <form> fragment that outerHTML-swaps the
// loading placeholder inside the open modal. Always 200: a swap that includes
// "auto-classify failed" is still useful to the user.
func (h *handler) enqueueClassify(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	q := req.URL.Query()
	it, err := h.lookupItem(req.Context(), q.Get("feed"), q.Get("guid"))
	if err != nil {
		// Best-effort fragment so the modal shows the error inline. Skip the
		// form entirely (user has no actionable item id without a successful
		// lookup); render a small note + cancel button.
		fmt.Fprintf(w, `<p class="modal-note">lookup failed: %s</p>
<div class="modal-actions">
  <button type="button" class="btn-cancel"
          hx-on:click="document.getElementById('modal').innerHTML=''">close</button>
</div>`, html.EscapeString(err.Error()))
		return
	}

	data := confirmNoRuleData{Item: it, Category: h.feedCategories[it.FeedName]}
	dirs, err := h.seedctl.ListDirs(req.Context(), "tv")
	if err != nil {
		slog.Warn("list_dirs_failed", "err", err)
		data.AutoPickFailed = true
	} else {
		res, err := h.ai.PickDir(req.Context(), it.Title, dirs.Dirs)
		if err != nil {
			slog.Warn("aiclassify_pickdir_failed", "title", it.Title, "err", err)
			data.AutoPickFailed = true
		} else if h.roots.TV != "" && res.ShowDir != "" && res.Season > 0 {
			data.ProposedSaveDir = filepath.Join(h.roots.TV, res.ShowDir, fmt.Sprintf("season%d", res.Season))
			data.ClaudePicked = true
		} else {
			data.AutoPickFailed = true
		}
	}

	if err := h.r.enqueueForm.ExecuteTemplate(w, "_enqueue_form.html", data); err != nil {
		slog.Error("render_enqueue_form", "err", err)
	}
}

// enqueueDo executes the enqueue: fetch the .torrent, compute the hash if
// missing, POST to seedctl with the user-confirmed save_dir, then re-render
// the table body so the row picks up its new "auto-enqueued" badge. On
// failure we return an error row so the user sees the cause inline instead
// of a silent no-op.
//
// The save_dir comes from the modal form (both rule-matched and no-rule
// paths post a "save_dir" field). An empty value means "don't send
// X-Save-Directory" — seedctl/rtorrent fall back to the default download
// dir.
func (h *handler) enqueueDo(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	q := req.URL.Query()
	it, err := h.lookupItem(req.Context(), q.Get("feed"), q.Get("guid"))
	if err != nil {
		h.renderEnqueueError(w, fmt.Sprintf("lookup: %s", err))
		return
	}
	if err := req.ParseForm(); err != nil {
		h.renderEnqueueError(w, fmt.Sprintf("parse form: %s", err))
		return
	}
	saveDir := req.PostForm.Get("save_dir")

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
		"feed", it.FeedName, "guid", it.GUID, "hash", hash,
		"save_dir", saveDir, "had_rule", it.MatchedRule != "")

	// re-render the table preserving whatever filter state the user had open.
	// The modal form uses hx-include="#search" so the search form's q +
	// feed_filter + include_enqueued are present in the POST body.
	h.renderRows(w, req.Context(), ItemsQuery{
		Q:               req.PostForm.Get("q"),
		Feed:            req.PostForm.Get("feed_filter"),
		IncludeEnqueued: req.PostForm.Get("include_enqueued") == "1",
		Limit:           200,
	})
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
	fmt.Fprintf(w, `<tr><td colspan="4" class="empty">enqueue failed: %s</td></tr>`,
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
