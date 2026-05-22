// Package main runs the seedctl mtls server.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	"github.com/atomicstack/torrent-manager/internal/rtorrent"
)

type rtorrentClient interface {
	Ping() (string, error)
	LoadTorrent(bytes []byte, knownHash, saveDir string) (string, error)
	GetStatus(hash string) (api.TorrentStatus, error)
	ListTorrents() ([]rtorrent.TorrentMetadata, error)
}

type handler struct {
	c     rtorrentClient
	items *itemsdb.DB
}

func newHandler(c rtorrentClient, items *itemsdb.DB) http.Handler {
	mux := http.NewServeMux()
	h := &handler{c: c, items: items}
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /v1/torrents", h.listTorrents)
	mux.HandleFunc("GET /v1/torrents/{hash}", h.getTorrent)
	mux.HandleFunc("POST /v1/torrents", h.postTorrent)
	mux.HandleFunc("POST /v1/items", h.upsertItems)
	mux.HandleFunc("GET /v1/items", h.listItems)
	mux.HandleFunc("GET /v1/items/export", h.exportItems)
	return mux
}

func (h *handler) listTorrents(w http.ResponseWriter, _ *http.Request) {
	rows, err := h.c.ListTorrents()
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}
	entries := make([]api.TorrentEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, api.TorrentEntry{Hash: r.Hash, Name: r.Name})
	}
	writeJSON(w, 200, api.TorrentListResponse{Torrents: entries})
}

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	v, err := h.c.Ping()
	if err != nil {
		writeJSON(w, 503, api.HealthResponse{OK: false})
		return
	}
	writeJSON(w, 200, api.HealthResponse{OK: true, RTorrentVersion: v})
}

func (h *handler) getTorrent(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToUpper(r.PathValue("hash"))
	st, err := h.c.GetStatus(hash)
	if errors.Is(err, rtorrent.ErrNotFound) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}
	writeJSON(w, 200, st)
}

func (h *handler) postTorrent(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToUpper(r.Header.Get("X-Info-Hash"))
	if hash == "" {
		http.Error(w, "missing X-Info-Hash header", 400)
		return
	}
	// short-circuit duplicates without reading body — saves ~1 round-trip when the
	// client neglected to check first.
	if _, err := h.c.GetStatus(hash); err == nil {
		writeJSON(w, 200, api.AddTorrentResponse{InfoHash: hash, Added: false})
		return
	} else if !errors.Is(err, rtorrent.ErrNotFound) {
		http.Error(w, "internal", 500)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", 400)
		return
	}
	saveDir := r.Header.Get("X-Save-Directory")
	gotHash, err := h.c.LoadTorrent(body, hash, saveDir)
	if err != nil {
		http.Error(w, "load failed", 422)
		return
	}
	writeJSON(w, 200, api.AddTorrentResponse{InfoHash: gotHash, Added: true})
}

func (h *handler) upsertItems(w http.ResponseWriter, r *http.Request) {
	var req api.ItemsUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rows := make([]itemsdb.Item, 0, len(req.Items))
	for _, it := range req.Items {
		rows = append(rows, itemsdb.Item{
			FeedName:    it.FeedName,
			GUID:        it.GUID,
			Title:       it.Title,
			Link:        it.Link,
			Enclosure:   it.Enclosure,
			InfoHash:    it.InfoHash,
			MatchedRule: it.MatchedRule,
			FirstSeenAt: time.Unix(it.FirstSeenAt, 0),
			LastSeenAt:  time.Unix(it.LastSeenAt, 0),
		})
	}
	if err := h.items.Upsert(r.Context(), rows); err != nil {
		slog.Error("upsert items", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, api.ItemsUpsertResponse{Upserted: len(rows)})
}

func (h *handler) listItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, err := h.items.List(r.Context(), itemsdb.ListQuery{
		Q:               q.Get("q"),
		Feed:            q.Get("feed"),
		IncludeEnqueued: q.Get("include_enqueued") == "1",
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		slog.Error("list items", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, api.ItemsListResponse{Items: itemRowsToRecords(rows)})
}

func (h *handler) exportItems(w http.ResponseWriter, r *http.Request) {
	rows, err := h.items.Export(r.Context())
	if err != nil {
		slog.Error("export items", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, api.ItemsListResponse{Items: itemRowsToRecords(rows)})
}

func itemRowsToRecords(in []itemsdb.Item) []api.ItemRecord {
	out := make([]api.ItemRecord, 0, len(in))
	for _, it := range in {
		out = append(out, api.ItemRecord{
			ID:            it.ID,
			FeedName:      it.FeedName,
			GUID:          it.GUID,
			Title:         it.Title,
			Link:          it.Link,
			Enclosure:     it.Enclosure,
			InfoHash:      it.InfoHash,
			MatchedRule:   it.MatchedRule,
			FirstSeenAt:   it.FirstSeenAt.Unix(),
			LastSeenAt:    it.LastSeenAt.Unix(),
			Enqueued:      it.Enqueued,
			EnqueueSource: string(it.EnqueueSource),
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
