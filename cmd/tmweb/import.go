// Manual-import flow: a "import torrent" header link opens a modal where
// the user pastes a torrent URL and picks a feed. tmweb fetches + parses
// the .torrent, upserts an items row via seedctl, primes the torrentcache
// with the fetched bytes (so the follow-up enqueue is one-fetch-total),
// and then hands off to the standard enqueue-confirm modal so the user
// reviews the save dir before the actual rtorrent load.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/atomicstack/torrent-manager/internal/torrent"
)

// importModalData feeds the _import.html template.
type importModalData struct {
	Feeds        []string
	SelectedFeed string
	URL          string
	ErrorMsg     string
}

// importForm renders the empty import modal — invoked by the header link
// via hx-get="/api/import-form" hx-target="#modal".
func (h *handler) importForm(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.r.importModal.Execute(w, importModalData{Feeds: h.feeds}); err != nil {
		slog.Error("render_import_form", "err", err)
	}
}

// importDo is the form's POST target. Validates inputs, fetches + parses
// the .torrent, upserts an items row, primes the torrent cache, then
// delegates to the standard enqueue-confirm renderer so the user lands
// on the same modal flow that rss-discovered items get.
func (h *handler) importDo(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := req.ParseForm(); err != nil {
		h.renderImportError(w, importModalData{Feeds: h.feeds, ErrorMsg: fmt.Sprintf("parse form: %s", err)})
		return
	}
	rawURL := strings.TrimSpace(req.PostForm.Get("url"))
	feedName := req.PostForm.Get("feed")
	data := importModalData{Feeds: h.feeds, URL: rawURL, SelectedFeed: feedName}

	if rawURL == "" || feedName == "" {
		data.ErrorMsg = "url and feed are both required"
		h.renderImportError(w, data)
		return
	}
	if _, err := url.Parse(rawURL); err != nil {
		data.ErrorMsg = fmt.Sprintf("invalid url: %s", err)
		h.renderImportError(w, data)
		return
	}
	if !h.isKnownFeed(feedName) {
		data.ErrorMsg = fmt.Sprintf("unknown feed %q", feedName)
		h.renderImportError(w, data)
		return
	}

	it, err := h.fetchAndIndex(req.Context(), feedName, rawURL)
	if err != nil {
		data.ErrorMsg = err.Error()
		h.renderImportError(w, data)
		return
	}

	// hand off to the standard confirm renderer. it'll pick the rule-
	// matched template when the title hit a watchlist regex, otherwise
	// the no-rule (✨/manual) modal.
	if it.MatchedRule != "" {
		h.confirmRuleMatched(w, req.Context(), it)
		return
	}
	h.confirmNoRule(w, req.Context(), it)
}

// fetchAndIndex does the actual fetch + parse + upsert sequence. Returns
// the freshly-indexed item record (as it'd come back from seedctl) so the
// caller can hand it straight to the confirm renderer.
func (h *handler) fetchAndIndex(ctx context.Context, feedName, rawURL string) (api.ItemRecord, error) {
	cookie := os.Getenv(h.cookieEnvFor(feedName))
	body, err := fetchTorrent(ctx, rawURL, cookie)
	if err != nil {
		return api.ItemRecord{}, fmt.Errorf("fetch: %w", err)
	}
	meta, err := torrent.Parse(body)
	if err != nil {
		return api.ItemRecord{}, fmt.Errorf("parse: %w", err)
	}
	hash, err := feed.ComputeHashFromTorrentBytes(body)
	if err != nil {
		return api.ItemRecord{}, fmt.Errorf("compute hash: %w", err)
	}
	hash = strings.ToUpper(hash)

	// match the title against the user's watchlist so the resulting row
	// gets the same matched_rule wiring as anything that came in via rss
	// poll — keeps the downstream modal flow identical.
	var matchedRule string
	if rule, ok := feed.Match(meta.Name, feedName, h.shows); ok {
		matchedRule = rule.Name
	}

	now := time.Now().Unix()
	it := api.ItemRecord{
		FeedName:    feedName,
		GUID:        hash,
		Title:       meta.Name,
		Link:        rawURL,
		InfoHash:    hash,
		MatchedRule: matchedRule,
		FirstSeenAt: now,
		LastSeenAt:  now,
		TotalBytes:  meta.TotalBytes,
		FileCount:   meta.FileCount,
		DirCount:    meta.DirCount(),
	}
	if err := h.seedctl.UpsertItems(ctx, []api.ItemRecord{it}); err != nil {
		return api.ItemRecord{}, fmt.Errorf("index: %w", err)
	}
	// seedctl merges an upsert into any existing row in the same feed with
	// the same info_hash (e.g. rss already indexed this torrent), and that
	// row keeps its own guid. read the stored row back so every follow-up
	// request — meta, cache key, the modal's enqueue POST — uses the guid
	// that actually exists. also picks up enqueued state + saved meta.
	stored, err := h.lookupItemByHash(ctx, feedName, hash)
	if err != nil {
		return api.ItemRecord{}, fmt.Errorf("index: read back: %w", err)
	}
	it = stored
	it.TotalBytes, it.FileCount, it.DirCount = meta.TotalBytes, meta.FileCount, meta.DirCount()
	// best-effort: the meta columns aren't part of the upsert payload yet,
	// so set them via /v1/items/meta. failure here doesn't block the
	// import — the modal still works, just without inline size + counts
	// until the user opens it (which would also populate meta).
	if err := h.seedctl.SetItemMeta(ctx, api.ItemMetaRequest{
		FeedName:   feedName,
		GUID:       it.GUID,
		InfoHash:   hash,
		TotalBytes: meta.TotalBytes,
		FileCount:  meta.FileCount,
		DirCount:   meta.DirCount(),
	}); err != nil {
		slog.Warn("import_set_meta_failed", "feed", feedName, "guid", it.GUID, "err", err)
	}

	// prime the cache so the follow-up confirm-modal → enqueue POST
	// reuses these bytes instead of hitting the tracker again.
	h.torrents.Put(feedName, it.GUID, &cachedTorrent{
		Bytes:    body,
		Meta:     meta,
		InfoHash: hash,
	})
	return it, nil
}

// isKnownFeed guards the form against a tampered feed name. The dropdown
// only offers configured feeds so a missing match means the client lied
// about the form value.
func (h *handler) isKnownFeed(name string) bool {
	return slices.Contains(h.feeds, name)
}

// renderImportError re-renders the import modal with an inline error so
// the user can adjust the url/feed and retry without losing what they
// typed.
func (h *handler) renderImportError(w http.ResponseWriter, data importModalData) {
	if err := h.r.importModal.Execute(w, data); err != nil {
		slog.Error("render_import_error", "err", err)
	}
}
