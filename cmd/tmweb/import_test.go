package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImportForm_RendersFeedOptions confirms the import modal's GET endpoint
// returns the form with all configured feeds populated in the select.
func TestImportForm_RendersFeedOptions(t *testing.T) {
	mux := muxFor(t, "http://unused", http.DefaultClient,
		[]string{"TrackerA (TV)", "TrackerB (Movies)"})
	req := httptest.NewRequest(http.MethodGet, "/api/import-form", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `<form hx-post="/api/import"`)
	assert.Contains(t, body, `name="url"`)
	assert.Contains(t, body, `name="feed"`)
	assert.Contains(t, body, `<option value="TrackerA (TV)">TrackerA (TV)</option>`)
	assert.Contains(t, body, `<option value="TrackerB (Movies)">TrackerB (Movies)</option>`)
	// progress strip is embedded so the user sees the shimmer when submitting.
	assert.Contains(t, body, `enqueue-progress`)
	assert.Contains(t, body, `step-1`)
}

// TestImportDo_HappyPath_FetchesParsesUpsertsAndReturnsConfirm covers the
// full server flow: a tracker stub serves a minimal .torrent, the seedctl
// stub records the upsert + meta calls + answers the post-upsert lookup,
// and the response is the enqueue-confirm modal so the user can pick a
// save dir.
func TestImportDo_HappyPath_FetchesParsesUpsertsAndReturnsConfirm(t *testing.T) {
	info := metainfo.Info{
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
		Name:        "Super.Mario.Galaxy.2026.1080p.WEB-DL",
		Length:      1,
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	torrentBytes, err := bencode.Marshal(mi)
	require.NoError(t, err)
	expectedHash := sha1Upper(infoBytes)

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrentBytes)
	}))
	t.Cleanup(tracker.Close)

	var (
		gotUpsert  api.ItemsUpsertRequest
		gotMeta    api.ItemMetaRequest
		upsertHit  bool
		metaHit    bool
	)
	seedctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/items":
			body, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(body, &gotUpsert))
			upsertHit = true
			_ = json.NewEncoder(w).Encode(api.ItemsUpsertResponse{Upserted: len(gotUpsert.Items)})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/items/meta":
			body, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(body, &gotMeta))
			metaHit = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/items":
			// confirmRuleMatched/confirmNoRule fetch the just-upserted item
			// back to populate the modal. Echo what we'd have stored.
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{{
				FeedName: "TrackerA (Movies)",
				GUID:     expectedHash,
				Title:    info.Name,
				Link:     tracker.URL + "/.torrent",
				InfoHash: expectedHash,
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(seedctl.Close)

	r, err := newRenderer()
	require.NoError(t, err)
	mux := newMux(
		NewSeedctl(seedctl.URL, seedctl.Client()),
		[]string{"TrackerA (Movies)"},
		nil,
		map[string]string{"TrackerA (Movies)": "movies"},
		nil,
		savedir.Roots{Movies: "/t/movies"},
		nil,
		r,
	)

	form := url.Values{}
	form.Set("url", tracker.URL+"/.torrent")
	form.Set("feed", "TrackerA (Movies)")
	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	assert.True(t, upsertHit, "POST /v1/items must have been called")
	assert.True(t, metaHit, "POST /v1/items/meta must have been called")
	require.Len(t, gotUpsert.Items, 1)
	it := gotUpsert.Items[0]
	assert.Equal(t, "TrackerA (Movies)", it.FeedName)
	assert.Equal(t, expectedHash, it.GUID, "guid uses info_hash so re-importing is idempotent")
	assert.Equal(t, expectedHash, it.InfoHash)
	assert.Equal(t, info.Name, it.Title)
	assert.Equal(t, tracker.URL+"/.torrent", it.Link)

	// the response is the enqueue-confirm modal — assert distinctive markers.
	body := rr.Body.String()
	assert.Contains(t, body, "<h2>enqueue", "response is the confirm/no-rule modal, not the import form")
	assert.Contains(t, body, `name="save_dir"`)
}

// TestImportDo_MissingFields_RendersErrorModal asserts an empty URL or feed
// keeps the user on the import form with an inline error rather than
// silently 200ing or crashing.
func TestImportDo_MissingFields_RendersErrorModal(t *testing.T) {
	mux := muxFor(t, "http://unused", http.DefaultClient, []string{"TrackerA (Movies)"})

	form := url.Values{}
	form.Set("url", "")
	form.Set("feed", "")
	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `<form hx-post="/api/import"`, "stays on the import form")
	assert.Contains(t, body, "modal-error")
	assert.Contains(t, body, "url and feed are both required")
}

// TestImportDo_UnknownFeed_RendersErrorModal guards against a tampered feed
// param the dropdown wouldn't normally let the user pick.
func TestImportDo_UnknownFeed_RendersErrorModal(t *testing.T) {
	mux := muxFor(t, "http://unused", http.DefaultClient, []string{"TrackerA (Movies)"})

	form := url.Values{}
	form.Set("url", "https://example.test/.torrent")
	form.Set("feed", "TrackerZ (Forbidden)")
	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "modal-error")
	assert.Contains(t, body, "unknown feed")
}

// TestRenderLayout_HasImportLink confirms the layout includes the
// header-action link that opens the import modal.
func TestRenderLayout_HasImportLink(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	body := executeLayout(t, r)
	assert.Contains(t, body, `hx-get="/api/import-form"`)
	assert.Contains(t, body, `hx-target="#modal"`)
	assert.Contains(t, body, "import torrent")
}

func executeLayout(t *testing.T, r *renderer) string {
	t.Helper()
	var sb strings.Builder
	require.NoError(t, r.layout.Execute(&sb, struct{ Feeds []string }{Feeds: []string{"TrackerA (TV)"}}))
	return sb.String()
}
