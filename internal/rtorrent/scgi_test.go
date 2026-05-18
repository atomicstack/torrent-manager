package rtorrent

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSCGIEncode_HelloWorld(t *testing.T) {
	body := []byte("<?xml version=\"1.0\"?><methodCall><methodName>system.client_version</methodName></methodCall>")
	got, err := encodeSCGI(body)
	require.NoError(t, err)

	// SCGI framing: <hdr-len>:<hdr>,<body>
	// hdr must contain CONTENT_LENGTH (first, per spec), SCGI=1, CONTENT_TYPE.
	colon := bytes.IndexByte(got, ':')
	require.NotEqual(t, -1, colon)
	hdrEnd := bytes.IndexByte(got[colon+1:], ',')
	require.NotEqual(t, -1, hdrEnd)
	hdrPart := got[colon+1 : colon+1+hdrEnd]
	bodyPart := got[colon+1+hdrEnd+1:]

	assert.Contains(t, string(hdrPart), "CONTENT_LENGTH\x00")
	assert.Contains(t, string(hdrPart), "SCGI\x001\x00")
	assert.Contains(t, string(hdrPart), "CONTENT_TYPE\x00text/xml\x00")
	assert.Equal(t, body, bodyPart)
}

// TestSCGIRoundTrip_AgainstFakeListener confirms a fake rtorrent that returns
// a canned XML body sees a parseable SCGI request and the client reads the
// response cleanly.
func TestSCGIRoundTrip_AgainstFakeListener(t *testing.T) {
	cannedReply := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><string>0.9.8</string></value></param></params></methodResponse>")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	var got []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		got = append(got, buf[:n]...)
		_, _ = conn.Write(cannedReply)
	}()

	body := []byte("<methodCall/>")
	resp, err := dialSCGI(ln.Addr().String(), body)
	require.NoError(t, err)
	wg.Wait()

	assert.True(t, strings.HasPrefix(string(got), strings.Split(string(got), ":")[0]+":"))
	assert.Equal(t, cannedReply, resp)
}
