package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagingSeedctl mimics seedctl's GET /v1/items semantics closely enough to
// catch lookups that depend on paging: rows are returned newest-first, the
// feed / guid / info_hash params filter exactly, and limit caps the page
// (default 100, like itemsdb.List).
func pagingSeedctl(t *testing.T, rows []api.ItemRecord) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/items" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		var out []api.ItemRecord
		// rows are stored oldest-first; serve newest-first.
		for i := len(rows) - 1; i >= 0; i-- {
			it := rows[i]
			if f := q.Get("feed"); f != "" && it.FeedName != f {
				continue
			}
			if g := q.Get("guid"); g != "" && it.GUID != g {
				continue
			}
			if h := q.Get("info_hash"); h != "" && it.InfoHash != h {
				continue
			}
			out = append(out, it)
			if len(out) == limit {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: out, Total: len(out)})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An item older than the newest 1000 in its feed must still open in the
// enqueue modal. lookupItem used to list the feed with limit=1000 and scan,
// so anything deeper came back "item not found".
func TestEnqueueConfirm_FindsItemBeyondNewest1000(t *testing.T) {
	var rows []api.ItemRecord
	for i := range 1500 {
		rows = append(rows, api.ItemRecord{
			FeedName: "HPJ (TV)",
			GUID:     fmt.Sprintf("https://tracker.test/d.php?r=%040d", i),
			Title:    fmt.Sprintf("Old.Show.S01E%04d.1080p", i),
			Link:     fmt.Sprintf("https://tracker.test/d.php?r=%040d", i),
		})
	}
	srv := pagingSeedctl(t, rows)
	mux := muxFor(t, srv.URL, srv.Client(), []string{"HPJ (TV)"})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed="+url.QueryEscape("HPJ (TV)")+"&guid="+url.QueryEscape(rows[0].GUID), nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "item not found")
	assert.Contains(t, body, "Old.Show.S01E0000.1080p")
}

// Importing a torrent that rss already indexed merges into the existing
// row (itemsdb dedups on info_hash), which keeps its rss guid. The modal
// the import hands off to must reference that stored guid — not the
// info_hash the import proposed — or the follow-up enqueue POST looks up
// a guid that doesn't exist.
func TestImportDo_MergedIntoRSSRow_ModalUsesStoredGUID(t *testing.T) {
	info := metainfo.Info{PieceLength: 16384, Pieces: make([]byte, 20), Name: "Merged.Show.S02E03.1080p", Length: 1}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	torrentBytes, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: infoBytes})
	require.NoError(t, err)
	hash := sha1Upper(infoBytes)

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrentBytes)
	}))
	t.Cleanup(tracker.Close)

	rssGUID := "https://tracker.test/d.php?r=" + strings.ToLower(hash)
	stored := api.ItemRecord{
		FeedName: "HPJ (TV)", GUID: rssGUID, Title: info.Name,
		Link: tracker.URL + "/.torrent", InfoHash: hash,
	}
	lister := pagingSeedctl(t, []api.ItemRecord{stored})
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/items":
			// the merge happens server-side; the stored row keeps rssGUID.
			_ = json.NewEncoder(w).Encode(api.ItemsUpsertResponse{Upserted: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/items/meta":
			w.WriteHeader(http.StatusNoContent)
		default:
			// proxy reads to the paging fake.
			resp, err := lister.Client().Get(lister.URL + r.URL.RequestURI())
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}
	}))
	t.Cleanup(seedctl.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(NewSeedctl(seedctl.URL, seedctl.Client()), []string{"HPJ (TV)"}, nil,
		map[string]string{"HPJ (TV)": "tv"}, nil, savedir.Roots{TV: "/t/tv"}, nil, r)

	form := url.Values{"url": {tracker.URL + "/.torrent"}, "feed": {"HPJ (TV)"}}
	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "guid="+url.QueryEscape(rssGUID), "modal posts back the stored guid")
	assert.NotContains(t, body, "guid="+hash, "modal must not reference the unstored info_hash guid")
}
