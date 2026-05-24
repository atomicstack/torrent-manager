package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTorrentCache_PutGet_RoundTrip(t *testing.T) {
	c := newTorrentCache()
	want := &cachedTorrent{Bytes: []byte("abc"), InfoHash: "DEADBEEF"}
	c.Put("TrackerA", "guid-1", want)

	got, ok := c.Get("TrackerA", "guid-1")
	require.True(t, ok)
	assert.Equal(t, want.Bytes, got.Bytes)
	assert.Equal(t, want.InfoHash, got.InfoHash)
}

func TestTorrentCache_Miss(t *testing.T) {
	c := newTorrentCache()
	_, ok := c.Get("TrackerA", "unknown")
	assert.False(t, ok)
}

func TestTorrentCache_KeyIncludesFeed(t *testing.T) {
	// same guid under different feeds must not collide. plausible in practice
	// because guid uniqueness is per-feed, not global.
	c := newTorrentCache()
	c.Put("TrackerA", "shared-guid", &cachedTorrent{Bytes: []byte("tracker-a")})
	c.Put("TrackerB", "shared-guid", &cachedTorrent{Bytes: []byte("tracker-b")})

	gotA, ok := c.Get("TrackerA", "shared-guid")
	require.True(t, ok)
	assert.Equal(t, []byte("tracker-a"), gotA.Bytes)
	gotB, ok := c.Get("TrackerB", "shared-guid")
	require.True(t, ok)
	assert.Equal(t, []byte("tracker-b"), gotB.Bytes)
}

func TestTorrentCache_KeepsForever(t *testing.T) {
	// the cache used to evict on a TTL + size cap; both are gone. inserting
	// many entries and reading them all back proves nothing gets cleaned up
	// automatically.
	c := newTorrentCache()
	for i := range 200 {
		c.Put("TrackerA", string(rune('a'+i%26))+string(rune('a'+i/26)), &cachedTorrent{Bytes: []byte{byte(i)}})
	}
	hits := 0
	for i := range 200 {
		if _, ok := c.Get("TrackerA", string(rune('a'+i%26))+string(rune('a'+i/26))); ok {
			hits++
		}
	}
	assert.Equal(t, 200, hits, "all 200 entries should still be present — no TTL, no size cap")
}

func TestTorrentCache_Delete(t *testing.T) {
	c := newTorrentCache()
	c.Put("TrackerA", "g1", &cachedTorrent{Bytes: []byte("x")})
	c.Delete("TrackerA", "g1")
	_, ok := c.Get("TrackerA", "g1")
	assert.False(t, ok)

	// idempotent — deleting again should not panic.
	c.Delete("TrackerA", "g1")
}
