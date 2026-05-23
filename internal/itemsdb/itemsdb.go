// Package itemsdb is the seedctl-side persistence layer for RSS-discovered
// torrents and enqueue history. It owns the sqlite file at
// /home/seedbox/etc/seedctl/items.db in production. Schema migrations are
// idempotent — Open is safe to call on every process start.
//
// The database is goroutine-safe via database/sql's connection pool.
// A small number of triggers keep items_fts (an FTS5 virtual table indexing
// title only) in sync with items so callers can do full-text search without
// maintaining the index themselves.
package itemsdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// Item is one RSS-discovered torrent record.
type Item struct {
	ID            int64
	FeedName      string
	GUID          string
	Title         string
	Link          string // the .torrent download url (enclosure-if-present, else feed's <link>)
	InfoHash      string // empty when unknown
	MatchedRule   string // empty when no rule matched
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
	Enqueued      bool   // populated by List; true if there's a matching row in enqueues
	EnqueueSource Source // SourceAuto | SourceManual | '' when Enqueued = false

	// Meta fields are 0 when no .torrent has been fetched + parsed for this
	// item yet. Populated by SetMeta from tmgr (auto-enqueue) or tmweb
	// (manual enqueue / ✨ classify). TotalBytes > 0 is the "have meta"
	// signal; FileCount/DirCount can both be 0 on a single-file torrent.
	TotalBytes int64
	FileCount  int
	DirCount   int
}

// Source identifies which code path recorded an enqueue. The empty value is
// not valid for RecordEnqueue but is returned by List for items that have no
// enqueues row.
type Source string

const (
	SourceAuto   Source = "auto"
	SourceManual Source = "manual"
)

// Enqueue is one row of the enqueues history table.
type Enqueue struct {
	InfoHash   string
	FeedName   string
	GUID       string
	Title      string
	SaveDir    string
	Source     Source
	EnqueuedAt time.Time
}

// ListQuery parameters for List.
type ListQuery struct {
	Q               string // fts5 query string; empty = list by recency
	Feed            string // exact feed_name match; empty = all feeds
	IncludeEnqueued bool   // false = hide items present in `enqueues`
	Limit           int    // 0 = 100 default
	Offset          int
}

// DB wraps a *sql.DB with a typed surface. Safe for concurrent use.
type DB struct {
	db *sql.DB
}

// Open opens (or creates + migrates) the items database at path.
func Open(path string) (*DB, error) {
	// WAL + busy timeout so writers don't immediately fail under read contention.
	dsn := path + "?_journal=WAL&_busy_timeout=5000&_foreign_keys=on"
	sdb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	// serialise writers through a single connection so concurrent Upsert callers
	// can't race on the wal write lock and hit SQLITE_BUSY in spite of the busy
	// timeout. write throughput here is in the low hundreds of rows per tick.
	sdb.SetMaxOpenConns(1)
	if _, err := sdb.Exec(schemaSQL); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := runMigrations(sdb); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db: sdb}, nil
}

// runMigrations evolves the schema in-place when an older on-disk shape is
// detected. Each migration is idempotent and gated on a column existence
// probe so multiple opens of the same file are safe.
func runMigrations(sdb *sql.DB) error {
	// Drop the deprecated `enclosure` column if a pre-rename db still has it.
	// Empirically every feed sets <link> == <enclosure> so the column was
	// duplicate data; storing both wasted ~6mb/day on the wire across the
	// snapshot upserts. Requires sqlite >= 3.35.
	var has int
	if err := sdb.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('items') WHERE name = 'enclosure'`,
	).Scan(&has); err != nil {
		return fmt.Errorf("probe enclosure column: %w", err)
	}
	if has > 0 {
		if _, err := sdb.Exec(`ALTER TABLE items DROP COLUMN enclosure`); err != nil {
			return fmt.Errorf("drop enclosure column: %w", err)
		}
	}
	// Add the meta columns to pre-existing databases. The schemaSQL above
	// covers fresh installs; this loop catches dbs created before meta
	// caching landed.
	for _, col := range []struct{ name, sqlType string }{
		{"total_bytes", "INTEGER"},
		{"file_count", "INTEGER"},
		{"dir_count", "INTEGER"},
	} {
		var n int
		if err := sdb.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('items') WHERE name = ?`, col.name,
		).Scan(&n); err != nil {
			return fmt.Errorf("probe %s column: %w", col.name, err)
		}
		if n == 0 {
			if _, err := sdb.Exec(fmt.Sprintf(`ALTER TABLE items ADD COLUMN %s %s`, col.name, col.sqlType)); err != nil {
				return fmt.Errorf("add %s column: %w", col.name, err)
			}
		}
	}
	return nil
}

// Close releases the underlying connection pool.
func (d *DB) Close() error { return d.db.Close() }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS items (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  feed_name      TEXT NOT NULL,
  guid           TEXT NOT NULL,
  title          TEXT NOT NULL,
  link           TEXT,
  info_hash      TEXT,
  matched_rule   TEXT,
  first_seen_at  INTEGER NOT NULL,
  last_seen_at   INTEGER NOT NULL,
  total_bytes    INTEGER,
  file_count     INTEGER,
  dir_count      INTEGER,
  UNIQUE(feed_name, guid)
);
CREATE INDEX IF NOT EXISTS items_hash     ON items(info_hash);
CREATE INDEX IF NOT EXISTS items_lastseen  ON items(last_seen_at DESC);
CREATE INDEX IF NOT EXISTS items_firstseen ON items(first_seen_at DESC, id);
CREATE INDEX IF NOT EXISTS items_feed     ON items(feed_name);

CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
  title,
  content='items',
  content_rowid='id',
  tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS items_ai AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title) VALUES (new.id, new.title);
END;
CREATE TRIGGER IF NOT EXISTS items_ad AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title) VALUES('delete', old.id, old.title);
END;
CREATE TRIGGER IF NOT EXISTS items_au AFTER UPDATE OF title ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title) VALUES('delete', old.id, old.title);
  INSERT INTO items_fts(rowid, title) VALUES (new.id, new.title);
END;

CREATE TABLE IF NOT EXISTS enqueues (
  info_hash    TEXT PRIMARY KEY,
  feed_name    TEXT, guid TEXT,
  title        TEXT, save_dir TEXT,
  source       TEXT NOT NULL,
  enqueued_at  INTEGER NOT NULL
);
`

// Upsert inserts items, or updates last_seen_at + mutable fields on conflict.
// All rows go through one transaction so fts5 triggers stay consistent.
func (d *DB) Upsert(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO items (feed_name, guid, title, link, info_hash, matched_rule, first_seen_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(feed_name, guid) DO UPDATE SET
  title        = excluded.title,
  link         = excluded.link,
  info_hash    = COALESCE(excluded.info_hash, items.info_hash),
  matched_rule = COALESCE(excluded.matched_rule, items.matched_rule),
  last_seen_at = excluded.last_seen_at
`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range items {
		// reject zero timestamps so an accidentally-uninitialised caller can't
		// silently park epoch-0 in first_seen_at (which then never recovers,
		// since the ON CONFLICT path deliberately leaves first_seen_at alone).
		if it.FirstSeenAt.IsZero() || it.LastSeenAt.IsZero() {
			return fmt.Errorf("upsert (%s, %s): FirstSeenAt and LastSeenAt are required", it.FeedName, it.GUID)
		}
		if _, err := stmt.ExecContext(ctx,
			it.FeedName, it.GUID, it.Title, it.Link,
			nullableString(it.InfoHash), nullableString(it.MatchedRule),
			it.FirstSeenAt.Unix(), it.LastSeenAt.Unix(),
		); err != nil {
			return fmt.Errorf("upsert (%s, %s): %w", it.FeedName, it.GUID, err)
		}
	}
	return tx.Commit()
}

// List returns items matching the query. Ordering: bm25 when Q != "", else
// last_seen_at desc. Limit defaults to 100.
func (d *DB) List(ctx context.Context, q ListQuery) ([]Item, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	var (
		where []string
		args  []any
		join  string
		order string
	)
	if q.Q != "" {
		join = "JOIN items_fts f ON f.rowid = i.id"
		where = append(where, "items_fts MATCH ?")
		args = append(args, fts5Query(q.Q))
		// fts5 hits ranked by relevance first; ties broken by feed order
		// (newest tick, then RSS position within the tick — see below).
		order = "bm25(items_fts), i.first_seen_at DESC, i.id ASC"
	} else {
		// Default order: mirror the RSS feed. first_seen_at DESC puts items
		// from later ticks first. Within a tick (same first_seen_at) the
		// items table id is the insertion order, which matches the RSS XML
		// order tmgr iterates — so id ASC keeps the feed's natural ranking
		// (newer-in-feed first because rss publishes newest items first).
		order = "i.first_seen_at DESC, i.id ASC"
	}
	if q.Feed != "" {
		where = append(where, "i.feed_name = ?")
		args = append(args, q.Feed)
	}
	if !q.IncludeEnqueued {
		where = append(where, "(i.info_hash IS NULL OR NOT EXISTS (SELECT 1 FROM enqueues e WHERE e.info_hash = i.info_hash))")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	query := fmt.Sprintf(`
SELECT i.id, i.feed_name, i.guid, i.title,
       COALESCE(i.link, ''),
       COALESCE(i.info_hash, ''), COALESCE(i.matched_rule, ''),
       i.first_seen_at, i.last_seen_at,
       COALESCE(i.total_bytes, 0), COALESCE(i.file_count, 0), COALESCE(i.dir_count, 0),
       CASE WHEN i.info_hash IS NOT NULL AND EXISTS (SELECT 1 FROM enqueues e WHERE e.info_hash = i.info_hash) THEN 1 ELSE 0 END AS enqueued,
       COALESCE((SELECT source FROM enqueues e WHERE e.info_hash = i.info_hash), '') AS enq_source
FROM items i %s %s ORDER BY %s LIMIT ? OFFSET ?
`, join, whereSQL, order)
	args = append(args, q.Limit, q.Offset)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list query: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var first, last int64
		var enqueued int
		if err := rows.Scan(&it.ID, &it.FeedName, &it.GUID, &it.Title,
			&it.Link,
			&it.InfoHash, &it.MatchedRule,
			&first, &last,
			&it.TotalBytes, &it.FileCount, &it.DirCount,
			&enqueued, &it.EnqueueSource); err != nil {
			return nil, err
		}
		it.FirstSeenAt = time.Unix(first, 0)
		it.LastSeenAt = time.Unix(last, 0)
		it.Enqueued = enqueued == 1
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetMeta records the parsed-torrent meta for one (feed, guid). Called
// after tmgr or tmweb fetches + parses a .torrent so the next List call
// returns the meta inline (and tmweb can ship it to the browser at page
// load). No-op when the row doesn't exist yet — callers rely on the
// RSS-side Upsert to land the row first.
//
// req.InfoHash is optional: when non-empty, fills in items.info_hash if
// the row is still missing it (e.g. TrackerB feeds don't carry the hash in
// the rss guid). Never overwrites a non-empty hash already on the row.
//
// FOOTGUN: req.TotalBytes / FileCount / DirCount are written
// unconditionally. Pass the actual parsed values — passing zeros (e.g.
// to backfill only the info_hash on an existing row) will clobber any
// meta already there. If you only want to update one field, read the
// row first and re-send the rest. (We deliberately don't gate on a
// sentinel here — the only legitimate caller is the post-parse path in
// tmgr/tmweb which always has all four.)
func (d *DB) SetMeta(ctx context.Context, req api.ItemMetaRequest) error {
	_, err := d.db.ExecContext(ctx, `
UPDATE items SET
  total_bytes = ?,
  file_count  = ?,
  dir_count   = ?,
  info_hash   = COALESCE(NULLIF(info_hash, ''), NULLIF(?, ''))
WHERE feed_name = ? AND guid = ?
`, req.TotalBytes, req.FileCount, req.DirCount, req.InfoHash, req.FeedName, req.GUID)
	return err
}

// Export dumps every row (used for tmgr bootstrap when its local mirror is empty).
func (d *DB) Export(ctx context.Context) ([]Item, error) {
	return d.List(ctx, ListQuery{IncludeEnqueued: true, Limit: 1 << 30})
}

// RecordEnqueue inserts (or replaces) an enqueues row.
func (d *DB) RecordEnqueue(ctx context.Context, e Enqueue) error {
	_, err := d.db.ExecContext(ctx, `
INSERT INTO enqueues (info_hash, feed_name, guid, title, save_dir, source, enqueued_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(info_hash) DO UPDATE SET
  save_dir    = excluded.save_dir,
  source      = excluded.source,
  enqueued_at = excluded.enqueued_at
`, e.InfoHash, e.FeedName, e.GUID, e.Title, e.SaveDir, e.Source, e.EnqueuedAt.Unix())
	return err
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fts5Query turns a user-typed search string into an FTS5 MATCH expression.
// Splits on anything FTS5's unicode61 tokenizer treats as a word boundary
// (whitespace, hyphen, dot, etc.) and wraps each surviving token with a
// trailing prefix wildcard, so typing `web-dl` or `guy mo` both work. This
// deliberately does not expose FTS5's operator syntax — the consumer is a
// mobile search box, not a query DSL.
func fts5Query(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p+"*")
	}
	return strings.Join(out, " ")
}
