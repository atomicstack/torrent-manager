package rtorrent

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// encodeSCGI wraps an XML-RPC body in SCGI framing.
// per https://python.ca/scgi/protocol.txt, CONTENT_LENGTH must be first.
func encodeSCGI(body []byte) ([]byte, error) {
	var hdr bytes.Buffer
	hdr.WriteString("CONTENT_LENGTH")
	hdr.WriteByte(0)
	hdr.WriteString(strconv.Itoa(len(body)))
	hdr.WriteByte(0)
	hdr.WriteString("SCGI")
	hdr.WriteByte(0)
	hdr.WriteString("1")
	hdr.WriteByte(0)
	hdr.WriteString("CONTENT_TYPE")
	hdr.WriteByte(0)
	hdr.WriteString("text/xml")
	hdr.WriteByte(0)

	var out bytes.Buffer
	out.WriteString(strconv.Itoa(hdr.Len()))
	out.WriteByte(':')
	out.Write(hdr.Bytes())
	out.WriteByte(',')
	out.Write(body)
	return out.Bytes(), nil
}

// dialSCGI sends a single SCGI request and returns the entire response.
// addr is "host:port" for TCP scgi.
func dialSCGI(addr string, body []byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial scgi: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline scgi: %w", err)
	}

	framed, err := encodeSCGI(body)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(framed); err != nil {
		return nil, fmt.Errorf("write scgi: %w", err)
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("read scgi: %w", err)
	}
	return resp, nil
}
