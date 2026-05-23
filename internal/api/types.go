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
	Enqueued      bool   `json:"enqueued,omitempty"`
	EnqueueSource string `json:"enqueue_source,omitempty"` // 'auto' | 'manual' | ''
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
type ItemsListResponse struct {
	Items []ItemRecord `json:"items"`
}
