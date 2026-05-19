// Package hashcache is a best-effort, file-backed set of (info hash, name)
// pairs known to be present on the seedctl side. tmgr consults it before
// issuing a per-item dedup GET; a positive cache lookup short-circuits the
// network round-trip.
//
// The on-disk format is a single sorted, pretty-printed JSON document:
//
//	{
//	  "torrents": [
//	    {"hash": "...", "name": "..."},
//	    ...
//	  ]
//	}
//
// Entries are sorted by Name (case-insensitive) then Hash so a human can
// diff the file usefully. Every Add and Populate rewrites the whole file
// atomically via tmp+rename; at our scale (low thousands of entries) this is
// trivial.
//
// Concurrency: not safe for concurrent access. The cronjob runs as a single
// pod with concurrencyPolicy=Forbid.
package hashcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// Entry is a single (hash, name) pair. Same shape as api.TorrentEntry so
// callers can convert without a struct copy.
type Entry = api.TorrentEntry

// Cache is an in-memory map keyed by both Hash and Name, with a JSON-on-disk
// backing file.
type Cache struct {
	path        string
	byHash      map[string]Entry
	byName      map[string]Entry // key = lowercased Name
	initialised bool             // true if the backing file existed with >=1 entry
}

type cacheFile struct {
	Torrents []Entry `json:"torrents"`
}

// New opens or creates a cache at path. The file is read once and its entries
// are loaded into memory. Initialised reports whether the file existed and
// held at least one entry at New time — callers should populate via the
// network if Initialised returns false.
func New(path string) (*Cache, error) {
	c := &Cache{
		path:   path,
		byHash: make(map[string]Entry),
		byName: make(map[string]Entry),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache dir: %w", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("read cache: %w", err)
	}
	if len(body) == 0 {
		return c, nil
	}
	var doc cacheFile
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse cache: %w", err)
	}
	for _, e := range doc.Torrents {
		c.put(e)
	}
	c.initialised = len(c.byHash) > 0
	return c, nil
}

// Initialised reports whether the cache had any entries when it was opened.
func (c *Cache) Initialised() bool {
	return c != nil && c.initialised
}

// Len returns the in-memory entry count.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	return len(c.byHash)
}

// HasHash reports whether hash (case-insensitive) is cached.
func (c *Cache) HasHash(hash string) bool {
	if c == nil {
		return false
	}
	_, ok := c.byHash[strings.ToUpper(strings.TrimSpace(hash))]
	return ok
}

// HasName reports whether name (case-insensitive) is cached. Useful for
// cross-tracker dedup when the same release shows up with a different info
// hash on a different feed.
func (c *Cache) HasName(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.byName[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// Add inserts (hash, name) and rewrites the file. A duplicate hash is a no-op.
// An invalid hash returns an error without changing on-disk or in-memory state.
func (c *Cache) Add(hash, name string) error {
	if c == nil {
		return nil
	}
	h := normaliseHash(hash)
	if h == "" {
		return fmt.Errorf("invalid hash %q", hash)
	}
	if _, dup := c.byHash[h]; dup {
		return nil
	}
	c.put(Entry{Hash: h, Name: name})
	return c.flush()
}

// Populate atomically replaces the cache with the given entries.
func (c *Cache) Populate(entries []Entry) error {
	if c == nil {
		return nil
	}
	c.byHash = make(map[string]Entry, len(entries))
	c.byName = make(map[string]Entry, len(entries))
	for _, e := range entries {
		c.put(e)
	}
	if err := c.flush(); err != nil {
		return err
	}
	c.initialised = len(c.byHash) > 0
	return nil
}

// put merges an entry into the in-memory maps without writing. Invalid hashes
// are silently skipped (consistent with the file-load tolerance).
func (c *Cache) put(e Entry) {
	h := normaliseHash(e.Hash)
	if h == "" {
		return
	}
	e.Hash = h
	c.byHash[h] = e
	if e.Name != "" {
		c.byName[strings.ToLower(e.Name)] = e
	}
}

// flush writes the sorted pretty JSON document atomically via tmp+rename.
func (c *Cache) flush() error {
	entries := make([]Entry, 0, len(c.byHash))
	for _, e := range c.byHash {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		li := strings.ToLower(entries[i].Name)
		lj := strings.ToLower(entries[j].Name)
		if li != lj {
			return li < lj
		}
		return entries[i].Hash < entries[j].Hash
	})
	doc := cacheFile{Torrents: entries}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	body = append(body, '\n')

	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write cache tmp: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename cache tmp: %w", err)
	}
	return nil
}

// normaliseHash returns the uppercase form of a 40-char hex hash. Anything
// shorter, longer, or non-hex returns the empty string.
func normaliseHash(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 40 {
		return ""
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return strings.ToUpper(s)
}
