package main

import (
	"sync"
	"time"

	"github.com/atomicstack/torrent-manager/internal/torrent"
)

// cachedTorrent is one entry in torrentCache. Bytes is the raw .torrent
// payload (suitable for re-uploading to seedctl on enqueue); Meta is the
// parsed view used to brief claude on the layout; InfoHash is the SHA-1 of
// the info dict (uppercase hex) — stashed so enqueueDo can skip recomputing
// when the item came in via classify.
type cachedTorrent struct {
	Bytes    []byte
	Meta     torrent.Metadata
	InfoHash string
	expireAt time.Time
}

// torrentCache is a tiny TTL+max-size cache keyed by (feed, guid).
//
// Sized for the classify → enqueue handoff: the user clicks ✨, classify
// fetches + parses + stashes the bytes, then enqueue retrieves them within
// seconds. We don't try to be a real long-lived blob store — entries past
// the TTL get dropped lazily on the next Get, and the size cap evicts the
// soonest-to-expire when we'd otherwise grow without bound.
//
// All operations are O(N) over items; that's fine at the configured cap
// (~50 entries). A heap or doubly-linked list would matter at 10k+.
type torrentCache struct {
	mu      sync.Mutex
	items   map[string]*cachedTorrent
	maxSize int
	ttl     time.Duration
	now     func() time.Time // injectable so tests can fast-forward
}

func newTorrentCache(maxSize int, ttl time.Duration) *torrentCache {
	return &torrentCache{
		items:   make(map[string]*cachedTorrent),
		maxSize: maxSize,
		ttl:     ttl,
		now:     time.Now,
	}
}

func cacheKey(feed, guid string) string { return feed + "|" + guid }

// Get returns the cached entry if present and not expired. Lazy expiry: a
// stale entry is evicted on the read attempt that finds it stale.
func (c *torrentCache) Get(feed, guid string) (*cachedTorrent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey(feed, guid)
	e, ok := c.items[k]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expireAt) {
		delete(c.items, k)
		return nil, false
	}
	return e, true
}

// Put stores an entry under (feed, guid), stamping it with the cache's ttl.
// When the cache is full it evicts the entry with the soonest expireAt
// (which is the "oldest" entry under our fixed-ttl-on-write scheme).
func (c *torrentCache) Put(feed, guid string, e *cachedTorrent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.expireAt = c.now().Add(c.ttl)
	c.items[cacheKey(feed, guid)] = e
	if len(c.items) > c.maxSize {
		var victim string
		var victimAt time.Time
		for k, v := range c.items {
			if victim == "" || v.expireAt.Before(victimAt) {
				victim = k
				victimAt = v.expireAt
			}
		}
		delete(c.items, victim)
	}
}

// Delete removes the entry for (feed, guid). No-op if absent. Called by
// enqueueDo after a successful seedctl post so we don't hang on to bytes
// the user has already committed.
func (c *torrentCache) Delete(feed, guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, cacheKey(feed, guid))
}
