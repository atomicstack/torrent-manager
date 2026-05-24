package main

import (
	"sync"

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
}

// torrentCache is a tiny keep-forever cache keyed by (feed, guid).
//
// Previously this had a TTL + size cap with eviction; the operator opted
// to keep entries indefinitely instead (cheap — each entry is ~100KB and
// the working set is bounded by the number of items the user has touched).
// The TTL/size-cap logic is intentionally absent; Delete still works for
// explicit eviction, but no path calls it automatically.
type torrentCache struct {
	mu    sync.Mutex
	items map[string]*cachedTorrent
}

func newTorrentCache() *torrentCache {
	return &torrentCache{
		items: make(map[string]*cachedTorrent),
	}
}

func cacheKey(feed, guid string) string { return feed + "|" + guid }

// Get returns the cached entry if present.
func (c *torrentCache) Get(feed, guid string) (*cachedTorrent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[cacheKey(feed, guid)]
	return e, ok
}

// Put stores an entry under (feed, guid). Subsequent Puts for the same key
// overwrite the prior entry.
func (c *torrentCache) Put(feed, guid string, e *cachedTorrent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[cacheKey(feed, guid)] = e
}

// Delete removes the entry for (feed, guid). No-op if absent. Kept for
// completeness — no current call site invokes it after the "keep forever"
// switch — but explicit eviction is still useful for ad-hoc maintenance.
func (c *torrentCache) Delete(feed, guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, cacheKey(feed, guid))
}
