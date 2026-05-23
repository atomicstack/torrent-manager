package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// Seedctl is a thin http client wrapping the seedctl json/binary endpoints
// tmweb needs: GET /healthz, GET /v1/items, GET /v1/torrents/{hash}, and
// POST /v1/torrents. The transport (and any mTLS configuration) is owned by
// the caller via the *http.Client.
type Seedctl struct {
	base string
	hc   *http.Client
}

// NewSeedctl builds a client against the given base URL using the supplied
// http client. The base URL is trimmed of any trailing slash so callers can
// pass either "https://host:port" or "https://host:port/".
func NewSeedctl(base string, hc *http.Client) *Seedctl {
	return &Seedctl{
		base: strings.TrimRight(base, "/"),
		hc:   hc,
	}
}

// ItemsQuery is the input to ListItems. The zero value lists everything
// (unfiltered, server defaults for paging).
type ItemsQuery struct {
	Q               string
	Feed            string
	IncludeEnqueued bool
	Limit           int
	Offset          int
}

// Healthz pings seedctl's /healthz endpoint. Returns (true, nil) iff the
// server replied 200 with {"ok": true}; any other status, transport error,
// or decode failure surfaces as (false, err).
func (s *Seedctl) Healthz(ctx context.Context) (bool, error) {
	u := s.base + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("seedctl GET %s: build request: %w", u, err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("seedctl GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("seedctl GET %s: status %d", u, resp.StatusCode)
	}
	var out api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("seedctl GET %s: decode: %w", u, err)
	}
	return out.OK, nil
}

// ListItems calls GET /v1/items with the encoded query. Empty filters are
// omitted from the query string; defaults are left to the server.
func (s *Seedctl) ListItems(ctx context.Context, q ItemsQuery) (api.ItemsListResponse, error) {
	values := url.Values{}
	if q.Q != "" {
		values.Set("q", q.Q)
	}
	if q.Feed != "" {
		values.Set("feed", q.Feed)
	}
	if q.IncludeEnqueued {
		values.Set("include_enqueued", "1")
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		values.Set("offset", strconv.Itoa(q.Offset))
	}
	u := s.base + "/v1/items"
	if encoded := values.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return api.ItemsListResponse{}, fmt.Errorf("seedctl GET %s: build request: %w", u, err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return api.ItemsListResponse{}, fmt.Errorf("seedctl GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.ItemsListResponse{}, fmt.Errorf("seedctl GET %s: status %d", u, resp.StatusCode)
	}
	var out api.ItemsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.ItemsListResponse{}, fmt.Errorf("seedctl GET %s: decode: %w", u, err)
	}
	return out, nil
}

// GetTorrentStatus calls GET /v1/torrents/{hash}. Returns (status, true, nil)
// when the torrent exists, (zero, false, nil) on 404, and an error for any
// other status or transport failure.
func (s *Seedctl) GetTorrentStatus(ctx context.Context, hash string) (api.TorrentStatus, bool, error) {
	u := s.base + "/v1/torrents/" + strings.ToUpper(hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return api.TorrentStatus{}, false, fmt.Errorf("seedctl GET %s: build request: %w", u, err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return api.TorrentStatus{}, false, fmt.Errorf("seedctl GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var st api.TorrentStatus
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			return api.TorrentStatus{}, false, fmt.Errorf("seedctl GET %s: decode: %w", u, err)
		}
		return st, true, nil
	case http.StatusNotFound:
		return api.TorrentStatus{}, false, nil
	default:
		return api.TorrentStatus{}, false, fmt.Errorf("seedctl GET %s: status %d", u, resp.StatusCode)
	}
}

// ListDirs hits GET /v1/dirs?root=<rootName>. Returns the immediate subdir
// names under the configured root. 404 from seedctl (unknown root) is
// surfaced as an empty list + nil error so callers can fall through to the
// "user enters it manually" path without a special-case error type.
func (s *Seedctl) ListDirs(ctx context.Context, rootName string) (api.DirsListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := fmt.Sprintf("%s/v1/dirs?root=%s", s.base, url.QueryEscape(rootName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return api.DirsListResponse{}, fmt.Errorf("seedctl GET %s: build request: %w", u, err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return api.DirsListResponse{}, fmt.Errorf("seedctl GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return api.DirsListResponse{Dirs: []string{}}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return api.DirsListResponse{}, fmt.Errorf("seedctl GET %s: status %d", u, resp.StatusCode)
	}
	var out api.DirsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.DirsListResponse{}, fmt.Errorf("seedctl GET %s: decode: %w", u, err)
	}
	return out, nil
}

// SetItemMeta records the parsed-torrent meta for one (feed, guid). Used
// by the ✨ classify and manual-enqueue paths after a fetch + parse, so
// the next page load ships the meta inline to the browser. Best-effort —
// callers log and continue on error rather than failing the user-visible
// enqueue.
func (s *Seedctl) SetItemMeta(ctx context.Context, feed, guid string, totalBytes int64, fileCount, dirCount int) error {
	u := s.base + "/v1/items/meta"
	body, err := json.Marshal(api.ItemMetaRequest{
		FeedName: feed, GUID: guid,
		TotalBytes: totalBytes, FileCount: fileCount, DirCount: dirCount,
	})
	if err != nil {
		return fmt.Errorf("seedctl POST %s: marshal: %w", u, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("seedctl POST %s: build request: %w", u, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("seedctl POST %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("seedctl POST %s: status %d", u, resp.StatusCode)
	}
	return nil
}

// PostTorrent uploads a .torrent body to POST /v1/torrents. The body is sent
// as-is (no gzip): .torrent files are already binary/opaque-to-deflate, so
// compression would inflate rather than shrink. The info hash and save dir
// are passed as headers per the seedctl contract.
func (s *Seedctl) PostTorrent(ctx context.Context, hash string, body []byte, saveDir string) (api.AddTorrentResponse, error) {
	if hash == "" {
		return api.AddTorrentResponse{}, errors.New("seedctl POST /v1/torrents: hash is required")
	}
	u := s.base + "/v1/torrents"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return api.AddTorrentResponse{}, fmt.Errorf("seedctl POST %s: build request: %w", u, err)
	}
	req.Header.Set("Content-Type", "application/x-bittorrent")
	req.Header.Set("X-Info-Hash", strings.ToUpper(hash))
	req.ContentLength = int64(len(body))
	if saveDir != "" {
		req.Header.Set("X-Save-Directory", saveDir)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return api.AddTorrentResponse{}, fmt.Errorf("seedctl POST %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// drain body for connection reuse, ignore content
		_, _ = io.Copy(io.Discard, resp.Body)
		return api.AddTorrentResponse{}, fmt.Errorf("seedctl POST %s: status %d", u, resp.StatusCode)
	}
	var out api.AddTorrentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.AddTorrentResponse{}, fmt.Errorf("seedctl POST %s: decode: %w", u, err)
	}
	return out, nil
}
