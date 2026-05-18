//go:build integration
// +build integration

package main

import (
	"testing"

	"github.com/atomicstack/torrent-manager/internal/tlsx"
)

// TestIntegration_RealMTLS wires a real tls.Listener with a stub seedctl + a
// real http test server for the rss feed, then runs Run() against them with
// a real ClientConfig.
func TestIntegration_RealMTLS(t *testing.T) {
	// 1. tlsx.GenerateTestCerts → server cert with SAN 127.0.0.1
	// 2. start a real tls.Listener serving a stub seedctl that records POSTs
	// 3. start a real http test server for the rss feed
	// 4. build Config with real tls client config (ClientConfig) pointed at the seedctl URL
	// 5. Run() against them; assert one matching item, one POST observed
	t.Skip("flesh out per the design spec's integration matrix")
	_ = tlsx.GenerateTestCerts
}
