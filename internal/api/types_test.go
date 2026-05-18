package api_test

import (
	"encoding/json"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddTorrentResponse_JSONRoundTrip(t *testing.T) {
	in := api.AddTorrentResponse{
		InfoHash: "abc123abc123abc123abc123abc123abc123abc1",
		Added:    true,
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"info_hash":"abc123abc123abc123abc123abc123abc123abc1","added":true}`, string(b))

	var out api.AddTorrentResponse
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestTorrentStatus_JSONRoundTrip(t *testing.T) {
	in := api.TorrentStatus{
		InfoHash: "deadbeef" + "0123456789abcdef0123456789abcdef01234567",
		Name:     "Some.Show.S01E01.1080p",
		Complete: true,
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"info_hash":"deadbeef0123456789abcdef0123456789abcdef01234567","name":"Some.Show.S01E01.1080p","complete":true}`, string(b))

	var out api.TorrentStatus
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestHealthResponse_JSONRoundTrip(t *testing.T) {
	in := api.HealthResponse{OK: true, RTorrentVersion: "0.9.8"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"rtorrent_version":"0.9.8"}`, string(b))

	var out api.HealthResponse
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}
