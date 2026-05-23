package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTorrentCache_PutGet_RoundTrip(t *testing.T) {
	c := newTorrentCache(10, 5*time.Minute)
	want := &cachedTorrent{Bytes: []byte("abc"), InfoHash: "DEADBEEF"}
	c.Put("TrackerA", "guid-1", want)

	got, ok := c.Get("TrackerA", "guid-1")
	require.True(t, ok)
	assert.Equal(t, want.Bytes, got.Bytes)
	assert.Equal(t, want.InfoHash, got.InfoHash)
}

func TestTorrentCache_Miss(t *testing.T) {
	c := newTorrentCache(10, 5*time.Minute)
	_, ok := c.Get("TrackerA", "unknown")
	assert.False(t, ok)
}

func TestTorrentCache_KeyIncludesFeed(t *testing.T) {
	// same guid under different feeds must not collide. plausible in practice
	// because guid uniqueness is per-feed, not global.
	c := newTorrentCache(10, 5*time.Minute)
	c.Put("TrackerA", "shared-guid", &cachedTorrent{Bytes: []byte("tracker-a")})
	c.Put("TrackerB", "shared-guid", &cachedTorrent{Bytes: []byte("tracker-b")})

	gotA, ok := c.Get("TrackerA", "shared-guid")
	require.True(t, ok)
	assert.Equal(t, []byte("tracker-a"), gotA.Bytes)
	gotB, ok := c.Get("TrackerB", "shared-guid")
	require.True(t, ok)
	assert.Equal(t, []byte("tracker-b"), gotB.Bytes)
}

func TestTorrentCache_TTLExpiry(t *testing.T) {
	clock := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	c := newTorrentCache(10, 5*time.Minute)
	c.now = func() time.Time { return clock }

	c.Put("TrackerA", "g1", &cachedTorrent{Bytes: []byte("x")})
	clock = clock.Add(4 * time.Minute) // still inside ttl
	_, ok := c.Get("TrackerA", "g1")
	assert.True(t, ok, "entry should still be live within the ttl")

	clock = clock.Add(2 * time.Minute) // 6m total — past 5m ttl
	_, ok = c.Get("TrackerA", "g1")
	assert.False(t, ok, "expired entry should not be returned")
}

func TestTorrentCache_EvictsWhenOverMaxSize(t *testing.T) {
	clock := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	c := newTorrentCache(2, 5*time.Minute)
	c.now = func() time.Time { return clock }

	c.Put("TrackerA", "a", &cachedTorrent{Bytes: []byte("a")})
	clock = clock.Add(1 * time.Minute) // a expires latest among the first two
	c.Put("TrackerA", "b", &cachedTorrent{Bytes: []byte("b")})
	clock = clock.Add(1 * time.Minute) // both already in; cache full
	// adding c pushes us over the cap → evict the entry with the soonest
	// expireAt, which is "a" (put first → expires first).
	c.Put("TrackerA", "c", &cachedTorrent{Bytes: []byte("c")})

	_, ok := c.Get("TrackerA", "a")
	assert.False(t, ok, "a should have been evicted as the soonest-to-expire entry")
	_, okB := c.Get("TrackerA", "b")
	assert.True(t, okB, "b should still be present")
	_, okC := c.Get("TrackerA", "c")
	assert.True(t, okC, "c should still be present (just inserted)")
}

func TestTorrentCache_Delete(t *testing.T) {
	c := newTorrentCache(10, 5*time.Minute)
	c.Put("TrackerA", "g1", &cachedTorrent{Bytes: []byte("x")})
	c.Delete("TrackerA", "g1")
	_, ok := c.Get("TrackerA", "g1")
	assert.False(t, ok)

	// idempotent — deleting again should not panic.
	c.Delete("TrackerA", "g1")
}
