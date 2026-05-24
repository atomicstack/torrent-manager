// Package api defines the wire types exchanged between tmgr (client) and seedctl
// (server) over HTTPS. Keep this package dependency-free so both binaries can
// import it without dragging in transitive deps.
package api

// AddTorrentResponse is returned by POST /v1/torrents.
type AddTorrentResponse struct {
	InfoHash string `json:"info_hash"`
	Added    bool   `json:"added"` // false if rtorrent already had this hash
}

// TorrentStatus is returned by GET /v1/torrents/{hash} when present.
type TorrentStatus struct {
	InfoHash string `json:"info_hash"`
	Name     string `json:"name"`
	Complete bool   `json:"complete"`
}

// HealthResponse is returned by GET /healthz.
type HealthResponse struct {
	OK              bool   `json:"ok"`
	RTorrentVersion string `json:"rtorrent_version,omitempty"`
}

// TorrentEntry is a single (hash, name) pair from a listing.
type TorrentEntry struct {
	Hash string `json:"hash"` // uppercase 40-char hex
	Name string `json:"name"` // info.name — single filename or root dir
}

// TorrentListResponse is returned by GET /v1/torrents. Ordering reflects
// rtorrent's view (main).
type TorrentListResponse struct {
	Torrents []TorrentEntry `json:"torrents"`
}

// ItemRecord is a single RSS-discovered item on the wire. Times are unix seconds.
type ItemRecord struct {
	ID            int64  `json:"id,omitempty"`
	FeedName      string `json:"feed_name"`
	GUID          string `json:"guid"`
	Title         string `json:"title"`
	Link          string `json:"link,omitempty"`
	InfoHash      string `json:"info_hash,omitempty"`
	MatchedRule   string `json:"matched_rule,omitempty"`
	FirstSeenAt   int64  `json:"first_seen_at"`
	LastSeenAt    int64  `json:"last_seen_at"`
	Enqueued       bool   `json:"enqueued,omitempty"`
	EnqueueSource  string `json:"enqueue_source,omitempty"` // 'auto' | 'manual' | ''
	// EnqueueSaveDir is the absolute on-disk path recorded at enqueue
	// time. Empty when the row isn't enqueued (or when the enqueue went
	// through with no explicit save_dir → rtorrent's default download
	// dir applied).
	EnqueueSaveDir string `json:"enqueue_save_dir,omitempty"`
	// Meta fields are 0 until something has fetched + parsed the .torrent
	// and called /v1/items/meta. TotalBytes > 0 is the "have meta" signal;
	// FileCount/DirCount can both be 0 on a single-file torrent.
	TotalBytes int64 `json:"total_bytes,omitempty"`
	FileCount  int   `json:"file_count,omitempty"`
	DirCount   int   `json:"dir_count,omitempty"`
}

// ItemMetaRequest is POST /v1/items/meta's body — the parsed-torrent meta
// for one (feed, guid) tuple, recorded after a fetch + parse. InfoHash is
// optional: feeds like TrackerA embed it in the GUID and the items row already
// has it post-Upsert, but feeds like TrackerB don't — so we let the post-
// parse path fill it in so the enqueued-badge join can find a match.
type ItemMetaRequest struct {
	FeedName   string `json:"feed_name"`
	GUID       string `json:"guid"`
	InfoHash   string `json:"info_hash,omitempty"`
	TotalBytes int64  `json:"total_bytes"`
	FileCount  int    `json:"file_count"`
	DirCount   int    `json:"dir_count"`
}

// ItemsUpsertRequest is POST /v1/items's body — a batch of items to insert/update.
type ItemsUpsertRequest struct {
	Items []ItemRecord `json:"items"`
}

// ItemsUpsertResponse is the reply for POST /v1/items.
type ItemsUpsertResponse struct {
	Upserted int `json:"upserted"`
}

// ItemsListResponse is the reply for GET /v1/items and /v1/items/export.
// Total is the count of items matching the filter independent of any
// Limit/Offset (i.e. the unpaginated row count) — used by the tmweb ui
// to render the page count and the "N rows matching filter" summary.
type ItemsListResponse struct {
	Items []ItemRecord `json:"items"`
	Total int          `json:"total,omitempty"`
}

// DirsListResponse is the reply for GET /v1/dirs?root=<name>. The names are
// immediate subdirectory basenames under the configured root, hidden entries
// (those with a leading ".") excluded, sorted ascending.
type DirsListResponse struct {
	Dirs []string `json:"dirs"`
}
