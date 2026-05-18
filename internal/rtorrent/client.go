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
// serialised via an internal mutex; rtorrent's xmlrpc handler is not robustly
// concurrent at low ops/sec and the project never needs parallel calls.
type Client struct {
	addr string
	mu   sync.Mutex
}

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

// Ping invokes system.client_version and returns it as a string.
func (c *Client) Ping() (string, error) {
	return c.call("system.client_version")
}

// LoadTorrent uploads raw .torrent bytes via load.raw_verbose. The caller
// supplies the precomputed info hash (sha1 of the bencoded info dict) so the
// return value identifies which torrent rtorrent now knows about; rtorrent's
// load.raw_verbose returns 0 on success without echoing the hash.
func (c *Client) LoadTorrent(data []byte, knownHash string) (string, error) {
	_, err := c.call("load.raw_verbose", xmlString(""), xmlBase64(data))
	if err != nil {
		return "", err
	}
	return strings.ToUpper(knownHash), nil
}

// GetStatus looks up a torrent by uppercase info hash. Returns ErrNotFound if
// rtorrent emits a -501 fault.
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
