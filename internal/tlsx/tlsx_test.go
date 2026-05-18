package tlsx_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerAndClientConfigs_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	certs := tlsx.GenerateTestCerts(t, dir)

	serverCfg, err := tlsx.ServerConfig(certs.CAPath, certs.ServerCertPath, certs.ServerKeyPath)
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = serverCfg
	srv.StartTLS()
	defer srv.Close()

	clientCfg, err := tlsx.ClientConfig(certs.CAPath, certs.ClientCertPath, certs.ClientKeyPath)
	require.NoError(t, err)

	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	resp, err := hc.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "ok", string(b))
}

func TestClientConfig_RejectedByUnknownCA(t *testing.T) {
	// server uses one CA; client uses a totally different one.
	serverDir, clientDir := t.TempDir(), t.TempDir()
	serverCerts := tlsx.GenerateTestCerts(t, serverDir)
	clientCerts := tlsx.GenerateTestCerts(t, clientDir)

	serverCfg, err := tlsx.ServerConfig(serverCerts.CAPath, serverCerts.ServerCertPath, serverCerts.ServerKeyPath)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = serverCfg
	srv.StartTLS()
	defer srv.Close()

	// client trusts its own CA, not the server's
	clientCfg, err := tlsx.ClientConfig(clientCerts.CAPath, clientCerts.ClientCertPath, clientCerts.ClientKeyPath)
	require.NoError(t, err)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}
	_, err = hc.Get(srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown authority")

	_ = tls.VersionTLS13 // touch import to avoid unused
}
