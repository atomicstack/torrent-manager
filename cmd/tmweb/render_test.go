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

// TestRenderRows_EmptyShowsPlaceholder ensures the "no matches" row is
// rendered when the upstream returns zero items, so the table doesn't get
// stuck on "loading…" after a search.
func TestRenderRows_EmptyShowsPlaceholder(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, api.ItemsListResponse{}))
	assert.Contains(t, buf.String(), "no matches")
}

// TestRenderRows_EnqueuedItemHasBadge ensures an auto-enqueued item gets the
// auto-coloured badge in the status cell.
func TestRenderRows_EnqueuedItemHasBadge(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, api.ItemsListResponse{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", Title: "Foo S01E01", InfoHash: "DEADBEEF", Enqueued: true, EnqueueSource: "auto", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
	}}))
	body := buf.String()
	assert.Contains(t, body, "Foo S01E01")
	assert.Contains(t, body, `badge-auto`)
	assert.Contains(t, body, `auto-enqueued`)
}

// TestRenderRows_NotEnqueuedHasNoBadge ensures we don't accidentally render
// a stray badge for items that haven't been enqueued.
func TestRenderRows_NotEnqueuedHasNoBadge(t *testing.T) {
	r, err := newRenderer()
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, r.rows.Execute(&buf, api.ItemsListResponse{Items: []api.ItemRecord{
		{FeedName: "TrackerA (TV)", Title: "Bar S01E01", FirstSeenAt: time.Now().Unix(), LastSeenAt: time.Now().Unix()},
	}}))
	body := buf.String()
	assert.Contains(t, body, "Bar S01E01")
	assert.NotContains(t, body, "badge")
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
