package itemsdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// completion tracking on the enqueues table. a row moves through:
//
//	pending   completed_at NULL, notified_at NULL  → seedctl polls rtorrent
//	completed completed_at set,  notified_at NULL  → streamed until acked
//	notified  notified_at set                       → done
//
// a hash that vanished from rtorrent before finishing jumps straight to
// notified (MarkGone) without ever being announced.

// PendingCompletionChecks returns the hashes seedctl should ask rtorrent about.
func (d *DB) PendingCompletionChecks(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT info_hash FROM enqueues WHERE completed_at IS NULL AND notified_at IS NULL ORDER BY enqueued_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MarkCompleted records that hash finished downloading and returns the
// completion as it will be streamed.
func (d *DB) MarkCompleted(ctx context.Context, hash, name string, at time.Time) (api.Completion, error) {
	res, err := d.db.ExecContext(ctx,
		`UPDATE enqueues SET completed_at = ?, torrent_name = ? WHERE info_hash = ?`,
		at.Unix(), name, hash)
	if err != nil {
		return api.Completion{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return api.Completion{}, fmt.Errorf("mark completed %s: no enqueues row", hash)
	}
	cs, err := d.completions(ctx, `e.info_hash = ?`, hash)
	if err != nil {
		return api.Completion{}, err
	}
	if len(cs) == 0 {
		return api.Completion{}, fmt.Errorf("mark completed %s: row vanished", hash)
	}
	return cs[0], nil
}

// MarkGone retires a hash rtorrent no longer knows about, without announcing it.
func (d *DB) MarkGone(ctx context.Context, hash string, at time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE enqueues SET notified_at = ? WHERE info_hash = ?`, at.Unix(), hash)
	return err
}

// UnnotifiedCompletions returns completed rows not yet acked, oldest first.
func (d *DB) UnnotifiedCompletions(ctx context.Context) ([]api.Completion, error) {
	return d.completions(ctx, `e.completed_at IS NOT NULL AND e.notified_at IS NULL`)
}

// AckCompletions marks completed hashes as announced. unknown or not-yet-
// completed hashes are ignored.
func (d *DB) AckCompletions(ctx context.Context, hashes []string, at time.Time) error {
	if len(hashes) == 0 {
		return nil
	}
	args := make([]any, 0, len(hashes)+1)
	args = append(args, at.Unix())
	for _, h := range hashes {
		args = append(args, h)
	}
	_, err := d.db.ExecContext(ctx, `
UPDATE enqueues SET notified_at = ?
WHERE completed_at IS NOT NULL AND info_hash IN (?`+strings.Repeat(",?", len(hashes)-1)+`)`, args...)
	return err
}

// completions selects completed rows matching where. the title comes from the
// enqueues row or, failing that (seedctl records enqueues by hash only), the
// items index.
func (d *DB) completions(ctx context.Context, where string, args ...any) ([]api.Completion, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT e.info_hash,
       COALESCE(e.torrent_name, ''),
       COALESCE(NULLIF(e.title, ''),
                (SELECT i.title FROM items i WHERE upper(i.info_hash) = e.info_hash LIMIT 1),
                ''),
       COALESCE(e.save_dir, ''),
       e.completed_at
FROM enqueues e
WHERE `+where+`
ORDER BY e.completed_at, e.info_hash`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Completion
	for rows.Next() {
		var c api.Completion
		var finished sql.NullInt64
		if err := rows.Scan(&c.InfoHash, &c.Name, &c.Title, &c.SaveDir, &finished); err != nil {
			return nil, err
		}
		c.FinishedAt = finished.Int64
		out = append(out, c)
	}
	return out, rows.Err()
}
