// Package main runs the seedctl mtls server.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/rtorrent"
)

type rtorrentClient interface {
	Ping() (string, error)
	LoadTorrent(bytes []byte, knownHash string) (string, error)
	GetStatus(hash string) (api.TorrentStatus, error)
}

type handler struct {
	c rtorrentClient
}

func newHandler(c rtorrentClient) http.Handler {
	mux := http.NewServeMux()
	h := &handler{c: c}
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /v1/torrents/{hash}", h.getTorrent)
	mux.HandleFunc("POST /v1/torrents", h.postTorrent)
	return mux
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
	gotHash, err := h.c.LoadTorrent(body, hash)
	if err != nil {
		http.Error(w, "load failed", 422)
		return
	}
	writeJSON(w, 200, api.AddTorrentResponse{InfoHash: gotHash, Added: true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
