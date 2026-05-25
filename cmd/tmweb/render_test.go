package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderLayout_IncludesFeedOptions verifies that feed names from the
// watchlist are rendered as <option> elements in the dropdown and that the
// htmx attributes wiring the search bar to /api/search are present.
func TestRenderLayout_IncludesFeedOptions(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.layout.Execute(&buf, struct{ Feeds []string }{Feeds: []string{"TrackerA (TV)", "TrackerB (TV)"}}))
	body := buf.String()
	assert.Contains(t, body, `<option value="TrackerA (TV)">TrackerA (TV)</option>`)
	assert.Contains(t, body, `<option value="TrackerB (TV)">TrackerB (TV)</option>`)
	assert.Contains(t, body, `hx-get="/api/search"`)
	// the empty-default option lets the user re-select "all feeds" after
	// filtering — make sure we still render it.
	assert.Contains(t, body, `<option value="">all feeds</option>`)
}

// TestRenderLayout_HasURLSyncScript verifies the layout ships the inline JS
// that reads URL params on load and writes them on form changes. The script
// itself isn't tested via execution (no DOM here), but its presence + the
// key identifiers it depends on are pinned.
func TestRenderLayout_HasURLSyncScript(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.layout.Execute(&buf, struct{ Feeds []string }{Feeds: []string{"TrackerA (TV)"}}))
	body := buf.String()
	assert.Contains(t, body, "URLSearchParams(location.search)", "URL read must happen on load")
	assert.Contains(t, body, "history.replaceState", "URL must update via replaceState on form change")
	assert.Contains(t, body, "form.q.value", "search query input is named 'q'")
	assert.Contains(t, body, "form.feed_filter.value", "feed dropdown is named 'feed_filter'")
	assert.Contains(t, body, "form.enqueued_filter.value", "enqueued filter is a select named 'enqueued_filter'")
	// the dropdown must offer the three filter modes.
	assert.Contains(t, body, `<option value="all" selected>all</option>`, "default is 'all'")
	assert.Contains(t, body, `<option value="enqueued">enqueued</option>`)
	assert.Contains(t, body, `<option value="">unenqueued</option>`)
}

// TestRenderLayout_URLSyncScriptComesAfterPerPageSelect pins the ordering
// invariant: the inline form-sync script touches form.elements.per_page, so
// the <select name="per_page" form="search"> element MUST be in the DOM
// before the script runs. The script is inline + synchronous, so document
// order is the only thing keeping the per_page element from being undefined
// when the script tries to wire up its change listener.
//
// If this test fails, the script will throw TypeError at load time,
// silently killing pagination clicks, sort-header bindings, and per_page
// URL seeding — exactly the regression we saw after the per_page select
// was moved out of the search bar into the pagination bar.
func TestRenderLayout_URLSyncScriptComesAfterPerPageSelect(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.layout.Execute(&buf, struct{ Feeds []string }{Feeds: []string{"TrackerA (TV)"}}))
	body := buf.String()
	selectIdx := bytes.Index([]byte(body), []byte(`<select name="per_page"`))
	scriptIdx := bytes.Index([]byte(body), []byte("form.elements.per_page"))
	require.NotEqual(t, -1, selectIdx, "per_page select must exist in the layout")
	require.NotEqual(t, -1, scriptIdx, "form-sync script must reference form.elements.per_page")
	assert.Less(t, selectIdx, scriptIdx,
		"per_page <select> must be parsed BEFORE the script that wires its change listener")
}

// TestRenderRows_EmptyShowsPlaceholder ensures the "no matches" row is
// rendered when the upstream returns zero items, so the table doesn't get
// stuck on "loading…" after a search.
func TestRenderRows_EmptyShowsPlaceholder(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, rowsTemplateData{}))
	assert.Contains(t, buf.String(), "no matches")
}

// TestRenderRows_EnqueuedItemHasBadge ensures an enqueued item gets the
// auto-coloured badge in the status cell.
func TestRenderRows_EnqueuedItemHasBadge(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, rowsTemplateData{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", Title: "Foo S01E01", InfoHash: "DEADBEEF", Enqueued: true, EnqueueSource: "auto", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
	}}))
	body := buf.String()
	assert.Contains(t, body, "Foo S01E01")
	assert.Contains(t, body, `badge-enqueued`)
	assert.Contains(t, body, `enqueued`)
}

// TestRenderRows_NotEnqueuedHasNoBadge ensures we don't accidentally render
// a stray badge for items that haven't been enqueued.
func TestRenderRows_NotEnqueuedHasNoBadge(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, rowsTemplateData{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", Title: "Bar S01E01", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
	}}))
	body := buf.String()
	assert.Contains(t, body, "Bar S01E01")
	assert.NotContains(t, body, "badge")
}

// TestRenderRows_TitleIsClickableModalTrigger covers the post-redesign
// behavior: the title cell itself fires the modal-open htmx request (no
// separate enqueue button column).
func TestRenderRows_TitleIsClickableModalTrigger(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, rowsTemplateData{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", GUID: "g1", Title: "Foo S01E01", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
	}}))
	body := buf.String()
	assert.Contains(t, body, "cell-title-link", "title cell now exposes a clickable link element")
	assert.Contains(t, body, `hx-get="/api/enqueue-confirm`, "click opens the enqueue modal")
	assert.Contains(t, body, `hx-target="#modal"`)
	assert.NotContains(t, body, "btn-enqueue", "row-level enqueue button is gone")
	assert.NotContains(t, body, "cell-action", "row-level action cell is gone")
}

// TestRenderRows_EmbedsCachedMetaScript verifies the rows fragment carries
// a <script>window.cachedMeta = ...</script> block populated from items
// with TotalBytes > 0, so the client has size + contents without an extra
// server round-trip.
func TestRenderRows_EmbedsCachedMetaScript(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, rowsTemplateData{
		Items: []api.ItemRecord{
			{FeedName: "TrackerA (TV)", GUID: "g1", Title: "Foo S01E01", TotalBytes: 1_610_612_736, FileCount: 12, DirCount: 3, FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
			{FeedName: "TrackerA (TV)", GUID: "g2", Title: "Bar S01E01", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
		},
		MetaMap: metaMapJSON([]api.ItemRecord{
			{FeedName: "TrackerA (TV)", GUID: "g1", TotalBytes: 1_610_612_736, FileCount: 12, DirCount: 3},
			{FeedName: "TrackerA (TV)", GUID: "g2"},
		}),
	}))
	body := buf.String()
	assert.Contains(t, body, "window.cachedMeta", "script must populate window.cachedMeta")
	assert.Contains(t, body, `"TrackerA (TV)|g1"`, "item with meta keys into the map by feed|guid")
	assert.Contains(t, body, `"bytes":1610612736`, "byte count emitted as a json number")
	assert.Contains(t, body, `"file_count":12`)
	assert.Contains(t, body, `"dir_count":3`)
	assert.NotContains(t, body, `"TrackerA (TV)|g2"`, "items without meta (TotalBytes==0) are omitted")
}

// TestHumanAgo_RelativeBuckets sanity-checks the coarse relative-time
// buckets used in the table.
func TestHumanAgo_RelativeBuckets(t *testing.T) {
	now := time.Now().Unix()
	assert.Equal(t, "just now", humanAgo(now-30))
	assert.Equal(t, "5m ago", humanAgo(now-5*60))
	assert.Equal(t, "3h ago", humanAgo(now-3*3600))
	assert.Equal(t, "2d ago", humanAgo(now-2*86400))
	assert.Equal(t, "?", humanAgo(0))
	assert.Equal(t, "?", humanAgo(now+60)) // future
}

// TestIsoUTC_FormatsTimestamp confirms the iso template func renders a unix
// timestamp as RFC3339 in UTC, and that the zero value collapses to "" so
// the title attribute disappears rather than displaying "1970-01-01T00:00:00Z".
func TestIsoUTC_FormatsTimestamp(t *testing.T) {
	// 2026-05-22T04:35:12Z in unix seconds
	assert.Equal(t, "2026-05-22T04:35:12Z", isoUTC(1779424512))
	assert.Equal(t, "", isoUTC(0))
}

// TestRenderRows_FirstSeenHasISOTooltip verifies the cell-seen span gets a
// title attribute carrying the iso8601 timestamp so the browser surfaces the
// exact moment on hover.
func TestRenderRows_FirstSeenHasISOTooltip(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, rowsTemplateData{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", Title: "Foo S01E01", FirstSeenAt: 1779424512, LastSeenAt: 1779424512},
	}}))
	body := buf.String()
	assert.Contains(t, body, `title="2026-05-22T04:35:12Z"`,
		"cell-seen span must carry an ISO8601 title for hover tooltip")
}
