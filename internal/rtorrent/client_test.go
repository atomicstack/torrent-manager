package rtorrent

import (
	"bytes"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRTorrent listens on a local tcp port and, for each connection, reads the
// scgi request body and returns whatever canned response the test asked for.
type fakeRTorrent struct {
	t        *testing.T
	ln       net.Listener
	replies  [][]byte // per-request replies, in order
	requests [][]byte // captured request bodies (xml-rpc payloads)
	mu       sync.Mutex
	idx      int
}

func newFakeRTorrent(t *testing.T, replies ...[]byte) *fakeRTorrent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeRTorrent{t: t, ln: ln, replies: replies}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeRTorrent) addr() string { return f.ln.Addr().String() }

func (f *fakeRTorrent) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 1<<20)
			n, _ := c.Read(buf)
			// strip scgi framing: skip past "<len>:<headers>,"
			data := buf[:n]
			if i := bytes.IndexByte(data, ','); i >= 0 {
				data = data[i+1:]
			}
			f.mu.Lock()
			f.requests = append(f.requests, append([]byte(nil), data...))
			i := f.idx
			f.idx++
			f.mu.Unlock()
			if i < len(f.replies) {
				_, _ = c.Write(f.replies[i])
			}
		}(conn)
	}
}

const okClientVersion = "Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><string>0.9.8</string></value></param></params></methodResponse>"

func TestClient_Ping(t *testing.T) {
	f := newFakeRTorrent(t, []byte(okClientVersion))
	c := NewClient(f.addr())
	v, err := c.Ping()
	require.NoError(t, err)
	assert.Equal(t, "0.9.8", v)
	require.Len(t, f.requests, 1)
	assert.Contains(t, string(f.requests[0]), "system.client_version")
}

func TestClient_LoadTorrent_ReturnsHash(t *testing.T) {
	// rtorrent returns "0" on load.raw_verbose success; we don't try to look up
	// the hash afterwards — we trust the caller's precomputed hash.
	loadOK := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><i4>0</i4></value></param></params></methodResponse>")
	f := newFakeRTorrent(t, loadOK)

	c := NewClient(f.addr())
	hash, err := c.LoadTorrent([]byte("fake-bencoded-bytes"), "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "")
	require.NoError(t, err)
	assert.Equal(t, "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", hash) // uppercased
	require.Len(t, f.requests, 1)
	assert.Contains(t, string(f.requests[0]), "load.raw_verbose")
	assert.Contains(t, string(f.requests[0]), "<base64>") // bytes are base64-encoded
}

func TestClient_LoadTorrent_WithSaveDir(t *testing.T) {
	loadOK := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><i4>0</i4></value></param></params></methodResponse>")
	f := newFakeRTorrent(t, loadOK)

	c := NewClient(f.addr())
	hash, err := c.LoadTorrent([]byte("fake-bencoded-bytes"), "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "/home/seedbox/t/tv/severance/season2")
	require.NoError(t, err)
	assert.Equal(t, "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", hash)
	require.Len(t, f.requests, 1)
	assert.Contains(t, string(f.requests[0]), "d.directory.set=/home/seedbox/t/tv/severance/season2")
}

func TestClient_GetStatus_NotFound(t *testing.T) {
	// d.name on a missing hash returns a fault.
	fault := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-501</i4></value></member><member><name>faultString</name><value><string>Could not find info-hash</string></value></member></struct></value></fault></methodResponse>")
	f := newFakeRTorrent(t, fault)
	c := NewClient(f.addr())
	_, err := c.GetStatus("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClient_GetStatus_Found(t *testing.T) {
	// d.name returns the torrent name, then d.complete returns "1" for complete.
	nameReply := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><string>Severance.S01E01.1080p</string></value></param></params></methodResponse>")
	completeReply := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><i4>1</i4></value></param></params></methodResponse>")
	f := newFakeRTorrent(t, nameReply, completeReply)
	c := NewClient(f.addr())
	st, err := c.GetStatus("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.NoError(t, err)
	assert.Equal(t, "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", st.InfoHash)
	assert.Equal(t, "Severance.S01E01.1080p", st.Name)
	assert.True(t, st.Complete)
	require.Len(t, f.requests, 2)
	assert.Contains(t, string(f.requests[0]), "d.name")
	assert.Contains(t, string(f.requests[1]), "d.complete")
}
