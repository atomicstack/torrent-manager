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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/aiclassify"
	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/atomicstack/torrent-manager/internal/torrent"
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
		Handler:           withLogging(newMux(seedctl, feeds, cookieEnvs, feedCats, shows, roots, ai, r)),
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

// computeCommonRoot returns the parent directory shared by the configured
// tv + movies roots, including a trailing path separator. Returns "" when
// the two roots don't share a parent (or none are configured) — callers
// degrade to showing absolute paths in that case.
func computeCommonRoot(roots savedir.Roots) string {
	var parents []string
	if roots.TV != "" {
		parents = append(parents, filepath.Dir(roots.TV))
	}
	if roots.Movies != "" {
		parents = append(parents, filepath.Dir(roots.Movies))
	}
	if len(parents) == 0 {
		return ""
	}
	p := parents[0]
	for _, q := range parents[1:] {
		if q != p {
			return ""
		}
	}
	return p + string(filepath.Separator)
}

// stripPrefix returns absPath with the handler's commonRoot prefix removed
// when it matches. Falls through to absPath when commonRoot is empty or the
// path doesn't sit under it (e.g. user already passing a different absolute
// path through the modal).
func (h *handler) stripPrefix(absPath string) string {
	if h.commonRoot == "" {
		return absPath
	}
	if rel, ok := strings.CutPrefix(absPath, h.commonRoot); ok {
		return rel
	}
	return absPath
}

// expandPrefix turns a relative save_dir (as typed in the modal) back into
// an absolute path. Absolute inputs and empty values pass through unchanged
// so a user can still type a fully-qualified path or leave the field blank.
func (h *handler) expandPrefix(rel string) string {
	if rel == "" {
		return ""
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	if h.commonRoot == "" {
		return rel
	}
	return h.commonRoot + rel
}

// collectCompletions fetches the existing show / movie dir basenames from
// seedctl and returns them as relative path entries (e.g. "tv/severance",
// "movies/Blade Runner 2049"). Used to populate the modal's autocomplete
// datalist. Failures are silent — autocomplete is optional UX.
func (h *handler) collectCompletions(ctx context.Context) []string {
	roots := []string{"tv", "movies"}
	var out []string
	for _, r := range roots {
		resp, err := h.seedctl.ListDirs(ctx, r)
		if err != nil {
			continue
		}
		for _, d := range resp.Dirs {
			out = append(out, r+"/"+d)
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
	// commonRoot is the parent directory shared by roots.TV and roots.Movies
	// (with trailing separator). When non-empty we strip it from save_dirs
	// before rendering the modal — the user only sees + types the part under
	// the common root — and expand it back to absolute on POST. Empty when
	// the two roots don't share a parent (degrades to absolute paths).
	commonRoot string
	// torrents holds the .torrent bytes + parsed meta + info hash for items
	// the user is currently classifying/enqueueing. populated by
	// enqueueClassify (so claude gets the layout) and consumed by enqueueDo
	// (so we don't refetch from the tracker twice in a row).
	torrents *torrentCache
	r        *renderer
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
		commonRoot:     computeCommonRoot(roots),
		// classify → enqueue handoff is normally a few seconds; 24h lets a
		// browsed-but-unqueued item sit overnight without a re-fetch. 50
		// entries × ~100KB/.torrent = ~5MiB worst case.
		torrents: newTorrentCache(50, 24*time.Hour),
		r:        r,
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
	if err := h.r.rows.Execute(w, rowsTemplateData{
		Items:   items.Items,
		MetaMap: metaMapJSON(items.Items),
	}); err != nil {
		slog.Error("render_rows", "err", err)
		http.Error(w, "render", http.StatusInternalServerError)
	}
}

// rowsTemplateData carries both the rows and a JSON object literal of
// per-item meta (only items with TotalBytes > 0). The script tag at the
// bottom of _rows.html merges this into window.cachedMeta so the client
// has size + contents available without re-querying the server.
type rowsTemplateData struct {
	Items   []api.ItemRecord
	MetaMap template.JS
}

// metaMapJSON encodes a {"feed|guid": {bytes,file_count,dir_count}} map
// as a JSON literal safe to drop into a <script> body. Items without meta
// (TotalBytes == 0) are omitted. Returns "{}" when no item carries meta.
func metaMapJSON(items []api.ItemRecord) template.JS {
	out := make(map[string]map[string]int64, len(items))
	for _, it := range items {
		if it.TotalBytes <= 0 {
			continue
		}
		out[it.FeedName+"|"+it.GUID] = map[string]int64{
			"bytes":      it.TotalBytes,
			"file_count": int64(it.FileCount),
			"dir_count":  int64(it.DirCount),
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
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
		h.confirmRuleMatched(w, req.Context(), it)
		return
	}
	h.confirmNoRule(w, req.Context(), it)
}

// confirmRuleData is what the rule-matched modal template receives. Shares
// the partial fields the no-rule data struct exposes (Item, Category, Meta,
// OOB) so a single _torrent_meta.html template can render the dl in both
// modals.
type confirmRuleData struct {
	Item        api.ItemRecord
	Category    string
	SaveDir     string
	CommonRoot  string
	Completions []string
	Meta        *torrentMeta
	OOB         bool
}

// confirmRuleMatched is the original rule-matched path: re-derive the
// save_dir from the watchlist rule and render the _confirm.html modal.
// The save_dir is rendered relative to the common root so the user sees
// "tv/severance/season2" instead of the full "/home/seedbox/t/tv/...".
func (h *handler) confirmRuleMatched(w http.ResponseWriter, ctx context.Context, it api.ItemRecord) {
	rule := h.findRule(it.MatchedRule)
	if rule == nil {
		renderModalError(w, "rule missing",
			fmt.Sprintf("watchlist no longer has rule %q. reload the page?", it.MatchedRule))
		return
	}
	saveDir, _ := savedir.Derive(h.roots, *rule, it.Title)
	data := confirmRuleData{
		Item:        it,
		Category:    h.feedCategories[it.FeedName],
		SaveDir:     h.stripPrefix(saveDir),
		CommonRoot:  h.commonRoot,
		Completions: h.collectCompletions(ctx),
	}
	// meta on the seedctl items row is the persistent cache — populated by
	// tmgr after auto-enqueue + by tmweb after ✨ classify or manual
	// enqueue. nil here means nothing has parsed this .torrent yet (user
	// can click ✨ to fetch on demand). avoids any per-modal-open tracker
	// hit.
	data.Meta = metaFromItem(it)
	if err := h.r.confirm.Execute(w, data); err != nil {
		slog.Error("render_confirm", "err", err)
	}
}

// confirmNoRuleData is the data the no-rule modal template receives.
//
// ClaudeAvailable controls whether the modal renders the small ✨ icon next
// to the save_dir input — only the tv path with a working ai client gets it.
// Clicking the icon fires /api/enqueue-classify, which returns just the
// _enqueue_form_row.html partial with the populated proposal.
//
// Meta is the parsed torrent payload (size + counts), populated only after
// the ✨ click has fetched + parsed the .torrent. nil on the initial modal
// render. OOB toggles the hx-swap-oob="true" attribute on the metadata dl
// — set true when rendering the dl as part of the classify response so it
// replaces the dl already in the modal.
type confirmNoRuleData struct {
	Item            api.ItemRecord
	Category        string
	ProposedSaveDir string
	ClaudePicked    bool
	AutoPickFailed  bool
	ClaudeAvailable bool
	CommonRoot      string
	Completions     []string
	Meta            *torrentMeta
	OOB             bool
}

// torrentMeta is the lightweight view the modal needs once we've fetched
// and parsed the .torrent. BytesHuman is pre-formatted (e.g. "1.50 GiB")
// so the template doesn't have to know about size unit logic. DirCount
// includes the wrapper info.name for multi-file torrents; single-file
// torrents have DirCount = 0 (no wrapper, the file itself is top-level).
type torrentMeta struct {
	Bytes      int64
	BytesHuman string
	FileCount  int
	DirCount   int
}

// toTorrentMeta builds the modal view from a parsed torrent. Counts unique
// internal directories via Metadata.DirCount (which includes the info.name
// wrapper for multi-file torrents).
func toTorrentMeta(m torrent.Metadata) *torrentMeta {
	return &torrentMeta{
		Bytes:      m.TotalBytes,
		BytesHuman: torrent.HumanBytes(m.TotalBytes),
		FileCount:  m.FileCount,
		DirCount:   m.DirCount(),
	}
}

// metaFromItem builds the modal view from the persisted meta on a seedctl
// item record. Returns nil when the row has no meta yet (TotalBytes == 0),
// so templates can use {{if .Meta}} to decide whether to render the rows.
func metaFromItem(it api.ItemRecord) *torrentMeta {
	if it.TotalBytes <= 0 {
		return nil
	}
	return &torrentMeta{
		Bytes:      it.TotalBytes,
		BytesHuman: torrent.HumanBytes(it.TotalBytes),
		FileCount:  it.FileCount,
		DirCount:   it.DirCount,
	}
}

// confirmNoRule renders the _confirm_norule.html modal with a category-
// specific proposed save_dir. movies pre-fills the movies root; tv leaves
// the field empty + offers a claude icon to ask for a suggestion on demand.
// other categories get an empty input with no proposals.
func (h *handler) confirmNoRule(w http.ResponseWriter, ctx context.Context, it api.ItemRecord) {
	category := h.feedCategories[it.FeedName]
	data := confirmNoRuleData{
		Item:        it,
		Category:    category,
		CommonRoot:  h.commonRoot,
		Completions: h.collectCompletions(ctx),
	}

	switch category {
	case "movies":
		// movies just live under roots.Movies — no subdir convention.
		data.ProposedSaveDir = h.stripPrefix(h.roots.Movies)
	case "tv":
		// Leave the field empty; the user clicks the claude icon to populate
		// it (or types something themselves). ClaudeAvailable controls the
		// icon's presence.
		data.ClaudeAvailable = h.ai != nil
	default:
		// "other" / unknown: empty field, no icon.
	}
	// meta on the seedctl items row is the persistent cache — see comment
	// in confirmRuleMatched for details.
	data.Meta = metaFromItem(it)
	if err := h.r.confirmNoRule.Execute(w, data); err != nil {
		slog.Error("render_confirm_norule", "err", err)
	}
}

// enqueueClassify streams progress to the ✨ classify button via SSE. each
// server-side step (fetching → parsing → caching → querying) emits a
// `step` event; the parsed torrent metadata is OOB-swapped into the
// modal's #torrent-meta dl as soon as parsing succeeds; and the final
// form-row-block HTML is delivered as a `form` event for the JS handler
// to swap into the open modal. Failures still result in a `form` event
// (with AutoPickFailed=true) so the user always gets an editable input
// back to fall through to manual entry.
func (h *handler) enqueueClassify(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "sse unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	send := func(event, data string) {
		writeSSEEvent(w, flusher, event, data)
	}
	sendStep := func(name string) { send("step", name) }

	q := req.URL.Query()
	it, err := h.lookupItem(req.Context(), q.Get("feed"), q.Get("guid"))
	if err != nil {
		send("form", fmt.Sprintf(
			`<div class="form-row-block"><p class="modal-note">lookup failed: %s</p></div>`,
			html.EscapeString(err.Error())))
		return
	}

	data := confirmNoRuleData{
		Item:            it,
		Category:        h.feedCategories[it.FeedName],
		ClaudeAvailable: h.ai != nil,
		CommonRoot:      h.commonRoot,
	}

	// fall through to the "auto-classify failed" form-row if we can't make
	// any forward progress (no api key, fetch fails, claude fails, etc.).
	fail := func() {
		data.AutoPickFailed = true
		h.sendClassifyForm(send, data)
	}

	if h.ai == nil {
		fail()
		return
	}

	entry, ok := h.torrents.Get(it.FeedName, it.GUID)
	if ok {
		// cache hit (e.g. the user already classified and is re-clicking
		// before the ttl expires) — emit the early steps for visual
		// consistency but skip the actual fetch/parse/hash work.
		sendStep("fetching")
		sendStep("parsing")
		sendStep("caching")
	} else {
		sendStep("fetching")
		cookie := os.Getenv(h.cookieEnvFor(it.FeedName))
		body, ferr := fetchTorrent(req.Context(), it.Link, cookie)
		if ferr != nil {
			slog.Warn("fetch_torrent_failed", "feed", it.FeedName, "guid", it.GUID, "err", ferr)
			fail()
			return
		}
		sendStep("parsing")
		meta, perr := torrent.Parse(body)
		if perr != nil {
			slog.Warn("parse_torrent_failed", "feed", it.FeedName, "guid", it.GUID, "err", perr)
			fail()
			return
		}
		hash := it.InfoHash
		if hash == "" {
			h2, herr := feed.ComputeHashFromTorrentBytes(body)
			if herr != nil {
				slog.Warn("compute_hash_failed", "err", herr)
				fail()
				return
			}
			hash = h2
		}
		sendStep("caching")
		entry = &cachedTorrent{Bytes: body, Meta: meta, InfoHash: hash}
		h.torrents.Put(it.FeedName, it.GUID, entry)
		if err := h.seedctl.SetItemMeta(req.Context(), api.ItemMetaRequest{
			FeedName: it.FeedName, GUID: it.GUID, InfoHash: hash,
			TotalBytes: meta.TotalBytes, FileCount: meta.FileCount, DirCount: meta.DirCount(),
		}); err != nil {
			slog.Warn("set_item_meta_failed", "feed", it.FeedName, "guid", it.GUID, "err", err)
		}
	}

	data.Meta = toTorrentMeta(entry.Meta)
	// emit the OOB meta dl now so size + contents appear in the modal
	// before claude even starts thinking — most of the user-perceived
	// latency from here on is the claude call.
	metaHTML, err := renderTemplate(h.r.torrentMeta, "_torrent_meta.html", withOOB(data))
	if err != nil {
		slog.Error("render_torrent_meta_oob", "err", err)
	} else {
		send("meta", metaHTML)
	}

	sendStep("querying")
	dirs, derr := h.seedctl.ListDirs(req.Context(), "tv")
	if derr != nil {
		slog.Warn("list_dirs_failed", "err", derr)
		fail()
		return
	}
	contents := aiclassify.TorrentContents{
		Name:       entry.Meta.Name,
		SingleFile: entry.Meta.SingleFile,
		Files:      filePaths(entry.Meta.Files),
	}
	res, cerr := h.ai.PickDir(req.Context(), it.Title, dirs.Dirs, contents)
	if cerr != nil {
		slog.Warn("aiclassify_pickdir_failed", "title", it.Title, "err", cerr)
		fail()
		return
	}
	if h.roots.TV == "" || res.ShowDir == "" {
		fail()
		return
	}
	// res.Subdir is "" when the torrent already carries its own season
	// folder; filepath.Join collapses empty components, so the joined
	// path is simply <tv>/<show> in that case.
	abs := filepath.Join(h.roots.TV, res.ShowDir, res.Subdir)
	data.ProposedSaveDir = h.stripPrefix(abs)
	data.ClaudePicked = true
	h.sendClassifyForm(send, data)
}

// sendClassifyForm renders the form-row-block partial into a `form` event,
// which the SSE client handler swaps into the open modal in place of the
// previous .form-row-block. Always called once, as the terminal event of
// the SSE stream.
func (h *handler) sendClassifyForm(send func(event, data string), d confirmNoRuleData) {
	body, err := renderTemplate(h.r.enqueueFormRow, "_enqueue_form_row.html", d)
	if err != nil {
		slog.Error("render_enqueue_form_row", "err", err)
		send("form", fmt.Sprintf(
			`<div class="form-row-block"><p class="modal-note">render failed: %s</p></div>`,
			html.EscapeString(err.Error())))
		return
	}
	send("form", body)
}

// withOOB clones data with the OOB flag set, so the same template can be
// reused both inline (initial modal render) and as an hx-swap-oob fragment
// (classify response).
func withOOB(d confirmNoRuleData) confirmNoRuleData {
	d.OOB = true
	return d
}

// renderTemplate executes the named template against data and returns the
// rendered string. Centralised so callers don't repeat the bytes.Buffer
// dance, and so SSE payloads (which need to be assembled before write)
// share one path with debug logging.
func renderTemplate(t *template.Template, name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// writeSSEEvent writes one SSE event frame and flushes immediately. Each
// line of `data` becomes its own "data: " field so multi-line HTML bodies
// are preserved (the spec joins data lines with '\n' before delivery).
func writeSSEEvent(w io.Writer, flusher http.Flusher, event, data string) {
	var sb strings.Builder
	if event != "" {
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteByte('\n')
	}
	for _, line := range strings.Split(data, "\n") {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	if _, err := w.Write([]byte(sb.String())); err != nil {
		return
	}
	flusher.Flush()
}

// ensureTorrentCached returns a cache entry for the item: a hit if the
// classify → enqueue handoff already fetched it, otherwise it fetches,
// parses, hashes, and stores the entry before returning it.
func (h *handler) ensureTorrentCached(ctx context.Context, it api.ItemRecord) (*cachedTorrent, error) {
	if e, ok := h.torrents.Get(it.FeedName, it.GUID); ok {
		return e, nil
	}
	cookie := os.Getenv(h.cookieEnvFor(it.FeedName))
	body, err := fetchTorrent(ctx, it.Link, cookie)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", it.Link, err)
	}
	meta, err := torrent.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse torrent: %w", err)
	}
	hash := it.InfoHash
	if hash == "" {
		h2, herr := feed.ComputeHashFromTorrentBytes(body)
		if herr != nil {
			return nil, fmt.Errorf("compute hash: %w", herr)
		}
		hash = h2
	}
	entry := &cachedTorrent{Bytes: body, Meta: meta, InfoHash: hash}
	h.torrents.Put(it.FeedName, it.GUID, entry)
	// best-effort: persist the meta on the seedctl items row so subsequent
	// page loads ship size/contents inline (and any tmgr-side auto-enqueue
	// for the same item later short-circuits the parse).
	if err := h.seedctl.SetItemMeta(ctx, api.ItemMetaRequest{
		FeedName: it.FeedName, GUID: it.GUID, InfoHash: hash,
		TotalBytes: meta.TotalBytes, FileCount: meta.FileCount, DirCount: meta.DirCount(),
	}); err != nil {
		slog.Warn("set_item_meta_failed", "feed", it.FeedName, "guid", it.GUID, "err", err)
	}
	return entry, nil
}

// filePaths flattens the parsed file list to the path slice claude expects.
// The cap-to-N happens inside aiclassify.PickDir so the prompt stays bounded
// regardless of caller behaviour.
func filePaths(fs []torrent.FileEntry) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

// enqueueDo executes the enqueue: fetch the .torrent, compute the hash if
// missing, POST to seedctl with the user-confirmed save_dir, then re-render
// the table body so the row picks up its new "enqueued" badge. On
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
	// modal posts a relative path (e.g. "tv/severance/season2"); expand back
	// to the absolute path seedctl/rtorrent expects.
	saveDir := h.expandPrefix(req.PostForm.Get("save_dir"))

	// reuse the .torrent bytes + info hash from the classify step when
	// available; only hit the tracker when there's no cache entry (e.g.
	// rule-matched modal, or classify TTL expired). fetchTorrent uses
	// http.DefaultClient because tracker URLs are served off public CA,
	// NOT mTLS. The cookie comes from the per-feed env var declared in
	// the watchlist (e.g. COOKIE_TRACKER_A for TrackerA feeds); task 15 mounts the
	// same tmgr-cookies secret into tmweb's deployment.
	entry, err := h.ensureTorrentCached(req.Context(), it)
	if err != nil {
		h.renderEnqueueError(w, err.Error())
		return
	}
	if _, err := h.seedctl.PostTorrent(req.Context(), entry.InfoHash, entry.Bytes, saveDir); err != nil {
		h.renderEnqueueError(w, fmt.Sprintf("seedctl add: %s", err))
		return
	}
	// torrent is now committed to seedctl; the cached bytes have served
	// their purpose and we can drop them eagerly rather than wait for ttl.
	h.torrents.Delete(it.FeedName, it.GUID)
	slog.Info("manual_enqueue_done",
		"feed", it.FeedName, "guid", it.GUID, "hash", entry.InfoHash,
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
