// Package hashcache is a tmgr-local sqlite mirror of (info_hash, name) pairs
// known to be present on the seedctl side. tmgr consults it before issuing a
// per-item dedup GET; a positive cache lookup short-circuits the network
// round-trip.
//
// The on-disk format is a small sqlite database. Schema and behavior are
// unchanged from the previous JSON-backed implementation; only the storage
// layout is different. tmgr handles the JSON→sqlite migration on startup.
//
// Concurrency: not safe for concurrent access. The cronjob runs as a single
// pod with concurrencyPolicy=Forbid.
package hashcache

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// NOTE: the github.com/mattn/go-sqlite3 driver registration lives at the
// binary root (cmd/tmgr/main.go for hashcache, cmd/seedctl/main.go for
// itemsdb) so the side effect is visible from main rather than buried in
// a library import.

// Entry is a single (hash, name) pair. Same shape as api.TorrentEntry so
// callers can convert without a struct copy.
type Entry = api.TorrentEntry

// Cache is a sqlite-backed (info hash, name) set.
type Cache struct {
	path        string
	db          *sql.DB
	initialised bool // true if the backing file existed with >=1 row at open time
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS items (
  feed_name      TEXT NOT NULL DEFAULT '',
  guid           TEXT NOT NULL DEFAULT '',
  title          TEXT NOT NULL,
  info_hash      TEXT,
  last_seen_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (feed_name, guid)
);
CREATE INDEX IF NOT EXISTS items_hash ON items(info_hash);
CREATE INDEX IF NOT EXISTS items_name ON items(title);
`

// New opens or creates a cache at path. Initialised reports whether the file
// existed and held at least one row at New time — callers should populate via
// the network if Initialised returns false.
func New(path string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache dir: %w", err)
	}
	dsn := path + "?_journal=WAL&_busy_timeout=5000&_foreign_keys=on"
	sdb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping cache: %w", err)
	}
	// serialise writers through a single connection; mirrors itemsdb.
	sdb.SetMaxOpenConns(1)
	if _, err := sdb.Exec(schemaSQL); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	c := &Cache{path: path, db: sdb}
	var n int
	if err := sdb.QueryRow("SELECT COUNT(*) FROM items").Scan(&n); err == nil {
		c.initialised = n > 0
	}
	return c, nil
}

// Initialised reports whether the cache had any rows when it was opened.
func (c *Cache) Initialised() bool {
	return c != nil && c.initialised
}

// Len returns the current row count.
func (c *Cache) Len() int {
	if c == nil || c.db == nil {
		return 0
	}
	var n int
	if err := c.db.QueryRow("SELECT COUNT(*) FROM items").Scan(&n); err != nil {
		return 0
	}
	return n
}

// FileSize returns the on-disk size of the backing file in bytes. Returns 0
// when the file is missing or unreadable.
func (c *Cache) FileSize() int64 {
	if c == nil {
		return 0
	}
	fi, err := os.Stat(c.path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// HasHash reports whether hash (case-insensitive) is cached.
func (c *Cache) HasHash(hash string) bool {
	if c == nil || c.db == nil {
		return false
	}
	h := normaliseHash(hash)
	if h == "" {
		return false
	}
	var present int
	err := c.db.QueryRow("SELECT 1 FROM items WHERE info_hash = ? LIMIT 1", h).Scan(&present)
	return err == nil
}

// HasName reports whether name (case-insensitive) is cached. Useful for
// cross-tracker dedup when the same release shows up with a different info
// hash on a different feed. Empty names never match.
func (c *Cache) HasName(name string) bool {
	if c == nil || c.db == nil {
		return false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	var present int
	err := c.db.QueryRow("SELECT 1 FROM items WHERE LOWER(title) = LOWER(?) LIMIT 1", name).Scan(&present)
	return err == nil
}

// Add inserts (hash, name). A duplicate hash is a no-op. An invalid hash
// returns an error without changing on-disk or in-memory state.
func (c *Cache) Add(hash, name string) error {
	if c == nil {
		return nil
	}
	h := normaliseHash(hash)
	if h == "" {
		return fmt.Errorf("invalid hash %q", hash)
	}
	// feed_name='' + guid=hash keeps each Add-row unique without needing a
	// real (feed, guid) tuple. INSERT OR IGNORE absorbs duplicate-hash adds.
	_, err := c.db.Exec(
		`INSERT OR IGNORE INTO items (feed_name, guid, title, info_hash, last_seen_at) VALUES ('', ?, ?, ?, 0)`,
		h, name, h,
	)
	if err != nil {
		return fmt.Errorf("insert cache row: %w", err)
	}
	return nil
}

// Populate atomically replaces the cache contents with the given entries.
// Invalid hashes are silently skipped (mirrors the file-load tolerance of the
// JSON-era implementation).
func (c *Cache) Populate(entries []Entry) error {
	if c == nil {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM items"); err != nil {
		return fmt.Errorf("clear cache: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO items (feed_name, guid, title, info_hash, last_seen_at) VALUES ('', ?, ?, ?, 0)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, e := range entries {
		h := normaliseHash(e.Hash)
		if h == "" {
			continue
		}
		if _, err := stmt.Exec(h, e.Name, h); err != nil {
			return fmt.Errorf("insert cache row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cache tx: %w", err)
	}
	// Match the old semantics: initialised iff at least one row landed.
	var n int
	if err := c.db.QueryRow("SELECT COUNT(*) FROM items").Scan(&n); err == nil {
		c.initialised = n > 0
	}
	return nil
}

// Close releases the underlying sqlite connection. Safe on a nil receiver.
func (c *Cache) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
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
