//go:build integration
// +build integration

package main

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_AddAndGet wires up the real handler with a fake scgi
// listener, real mtls certs, and drives requests over a tls.Listener bound to
// 127.0.0.1:0.
func TestIntegration_AddAndGet(t *testing.T) {
	// 1. fake scgi listener canned to respond OK to anything (system.client_version, load.raw_verbose, d.name, d.complete)
	// 2. real handler wired to rtorrent.NewClient pointed at the fake scgi addr
	// 3. real tls.Listener wrapping the handler with ServerConfig from tlsx.GenerateTestCerts
	// 4. client using ClientConfig drives POST /v1/torrents and GET /v1/torrents/{hash}
	// see the unit-test stubClient and the rtorrent fakeRTorrent for the building blocks.

	t.Skip("Compose fakeRTorrent + handler + tls.Listener — flesh out during implementation. " +
		"Required cases: valid client cert + POST → scgi receives load.raw_verbose; " +
		"expired client cert → 401; CA mismatch → handshake fails; " +
		"rtorrent unreachable → 503 on /healthz.")
	_ = bytes.NewReader
	_ = tls.VersionTLS13
	_ = tlsx.GenerateTestCerts
	_ = http.DefaultClient
	_ = require.New
	_ = assert.New
}
