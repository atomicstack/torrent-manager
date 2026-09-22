package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/atomicstack/torrent-manager/internal/aiclassify"
	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noRuleTVModal renders the no-rule tv enqueue modal for one item, with a
// configured (never actually called) claude client.
func noRuleTVModal(t *testing.T, it api.ItemRecord) string {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/items" {
			_ = json.NewEncoder(w).Encode(api.ItemsListResponse{Items: []api.ItemRecord{it}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stub.Close)
	r, err := newRenderer()
	require.NoError(t, err)
	ai := aiclassify.New("test", option.WithBaseURL("http://127.0.0.1:1"))
	mux := newMux(NewSeedctl(stub.URL, stub.Client()), []string{it.FeedName}, nil,
		map[string]string{it.FeedName: "tv"}, nil, savedir.Roots{TV: "/t/tv"}, ai, r)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/enqueue-confirm?feed=TrackerB+%28TV%29&guid=g1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	return rr.Body.String()
}

// opening a no-rule tv item for enqueue (from a row or straight out of an
// import) kicks off the claude classify automatically — the ✨ button is
// flagged so the client starts the sse stream as soon as the modal lands.
func TestEnqueueConfirm_NoRule_TV_AutoStartsClassify(t *testing.T) {
	body := noRuleTVModal(t, api.ItemRecord{
		FeedName: "TrackerB (TV)", GUID: "g1", Title: "Severance.S02E01.1080p",
		FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix(),
	})
	assert.Contains(t, body, "btn-classify")
	assert.Contains(t, body, "data-auto-classify", "classify must start without a click")
}

// already-enqueued items have no ✨ at all, so nothing auto-starts.
func TestEnqueueConfirm_NoRule_TV_Enqueued_NoAutoClassify(t *testing.T) {
	body := noRuleTVModal(t, api.ItemRecord{
		FeedName: "TrackerB (TV)", GUID: "g1", Title: "Severance.S02E01.1080p",
		Enqueued: true, EnqueueSaveDir: "/t/tv/severance/season2",
		FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix(),
	})
	assert.NotContains(t, body, "data-auto-classify")
}

// the form-row the classify stream hands back must not re-arm the auto
// start, or the swapped-in row would immediately classify again (loop).
func TestClassifyFormRow_NeverAutoClassifies(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	out, err := renderTemplate(r.enqueueFormRow, "_enqueue_form_row.html", confirmNoRuleData{
		Item: api.ItemRecord{FeedName: "TrackerB (TV)", GUID: "g1"}, Category: "tv",
		ClaudeAvailable: true, ClaudePicked: true, ProposedSaveDir: "tv/severance/season2",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "btn-classify", "✨ stays available for a manual re-run")
	assert.NotContains(t, out, "data-auto-classify")
}

// the layout wires the auto-start: new content carrying a flagged ✨ button
// triggers startClassify on it.
func TestLayout_AutoClassifyHook(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	body := executeLayout(t, r)
	assert.Contains(t, body, "htmx:load")
	assert.Contains(t, body, "[data-auto-classify]")
}
