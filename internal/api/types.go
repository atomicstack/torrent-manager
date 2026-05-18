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
