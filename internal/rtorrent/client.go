package rtorrent

import (
	"errors"
	"strings"
	"sync"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// ErrNotFound is returned by GetStatus when rtorrent doesn't have the hash.
var ErrNotFound = errors.New("rtorrent: torrent not found")

// Client is a goroutine-safe rtorrent XML-RPC client. Concurrent callers are
// serialised via an internal mutex held for the duration of one .call();
// multi-call methods like GetStatus are NOT atomic across calls. The mutex's
// purpose is to keep one rtorrent xmlrpc invocation from racing another at
// the wire level — rtorrent's handler is not robustly concurrent at low ops/sec
// — not to provide higher-level transaction semantics.
//
// Always pass *Client around, never Client by value: the embedded noCopy
// sentinel makes `go vet` catch accidental copies (which would duplicate
// the mutex and break the serialisation guarantee).
type Client struct {
	_    noCopy
	addr string
	mu   sync.Mutex
}

// noCopy marks Client as non-copyable post-construction. go vet's copylocks
// check picks up the Lock/Unlock methods and flags any value copy. Same
// pattern sync.WaitGroup, sync.Mutex, and strings.Builder use internally.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// NewClient returns a Client that dials addr (tcp host:port) for each call.
func NewClient(addr string) *Client {
	return &Client{addr: addr}
}

func (c *Client) call(method string, args ...xmlValue) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := dialSCGI(c.addr, []byte(encodeXMLRPCCall(method, args...)))
	if err != nil {
		return "", err
	}
	return decodeXMLRPCResponse(resp)
}

// callStringMatrix runs an xmlrpc method that returns an array-of-arrays.
func (c *Client) callStringMatrix(method string, args ...xmlValue) ([][]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := dialSCGI(c.addr, []byte(encodeXMLRPCCall(method, args...)))
	if err != nil {
		return nil, err
	}
	return decodeXMLRPCResponseStringMatrix(resp)
}

// TorrentMetadata is a (hash, name, directory) tuple returned by
// ListTorrents. Hash is uppercase hex; Name is info.name (the .torrent's
// single filename or root directory); Directory is rtorrent's download
// directory for the torrent (absolute path, including a trailing
// info.name for multi-file torrents).
type TorrentMetadata struct {
	Hash      string
	Name      string
	Directory string
}

// ListTorrents invokes d.multicall2 to fetch every torrent's hash, name,
// and download directory in one round-trip. Ordering reflects rtorrent's
// view (main).
func (c *Client) ListTorrents() ([]TorrentMetadata, error) {
	rows, err := c.callStringMatrix("d.multicall2",
		xmlString(""),    // arg 1: target rtorrent (empty = current)
		xmlString("main"), // arg 2: view to enumerate
		xmlString("d.hash="),
		xmlString("d.name="),
		xmlString("d.directory="),
	)
	if err != nil {
		return nil, err
	}
	out := make([]TorrentMetadata, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		out = append(out, TorrentMetadata{
			Hash:      strings.ToUpper(row[0]),
			Name:      row[1],
			Directory: row[2],
		})
	}
	return out, nil
}

// Ping invokes system.client_version and returns it as a string.
func (c *Client) Ping() (string, error) {
	return c.call("system.client_version")
}

// LoadTorrent uploads raw .torrent bytes via load.raw_start_verbose, which
// both loads AND starts the torrent atomically. The caller supplies the
// precomputed info hash (sha1 of the bencoded info dict) so the return value
// identifies which torrent rtorrent now knows about; load.raw_start_verbose
// returns 0 on success without echoing the hash.
// If saveDir is non-empty, sets the torrent's download directory via
// d.directory.set; rtorrent (and the filesystem) create intermediate dirs
// as needed.
func (c *Client) LoadTorrent(data []byte, knownHash, saveDir string) (string, error) {
	args := []xmlValue{xmlString(""), xmlBase64(data)}
	if saveDir != "" {
		args = append(args, xmlString("d.directory.set="+saveDir))
	}
	_, err := c.call("load.raw_start_verbose", args...)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(knownHash), nil
}

// GetStatus looks up a torrent by uppercase info hash. Returns ErrNotFound when
// the d.name call's fault string matches rtorrent's "could not find info-hash"
// pattern (either via faultCode -501 or the substring "not find"). Note: the
// two underlying calls (d.name then d.complete) are NOT atomic — a concurrent
// LoadTorrent or removal could land between them.
func (c *Client) GetStatus(hash string) (api.TorrentStatus, error) {
	hash = strings.ToUpper(hash)
	name, err := c.call("d.name", xmlString(hash))
	if err != nil {
		if strings.Contains(err.Error(), "-501") || strings.Contains(strings.ToLower(err.Error()), "not find") {
			return api.TorrentStatus{}, ErrNotFound
		}
		return api.TorrentStatus{}, err
	}
	complete, err := c.call("d.complete", xmlString(hash))
	if err != nil {
		return api.TorrentStatus{}, err
	}
	return api.TorrentStatus{
		InfoHash: hash,
		Name:     name,
		Complete: complete == "1",
	}, nil
}
