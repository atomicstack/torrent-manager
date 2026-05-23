package feed_test

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/atomicstack/torrent-manager/internal/feed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractHash_FromGUID(t *testing.T) {
	it := feed.Item{
		GUID: "https://tracker/torrent/aabbccddeeff00112233445566778899aabbccdd",
	}
	h, ok := feed.ExtractHash(it)
	assert.True(t, ok)
	assert.Equal(t, "AABBCCDDEEFF00112233445566778899AABBCCDD", h)
}

func TestExtractHash_FromLink(t *testing.T) {
	it := feed.Item{
		Link: "https://tracker/torrent/CAFEBABE1234567890abcdef1234567890abcdef",
	}
	h, ok := feed.ExtractHash(it)
	assert.True(t, ok)
	assert.Equal(t, "CAFEBABE1234567890ABCDEF1234567890ABCDEF", h)
}

func TestExtractHash_NoMatch(t *testing.T) {
	it := feed.Item{GUID: "no-hex-anywhere", Link: "also nope"}
	_, ok := feed.ExtractHash(it)
	assert.False(t, ok)
}

func TestExtractHash_GUIDWinsOverLink(t *testing.T) {
	// both fields carry distinct hashes; guid must win.
	it := feed.Item{
		GUID: "https://tracker/torrent/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Link: "https://tracker/torrent/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	h, ok := feed.ExtractHash(it)
	require.True(t, ok)
	assert.Equal(t, strings.Repeat("A", 40), h)
}

func TestComputeHashFromTorrentBytes(t *testing.T) {
	info := metainfo.Info{
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
		Name:        "test-file",
		Length:      3,
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)

	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	full, err := bencode.Marshal(mi)
	require.NoError(t, err)

	h := sha1.Sum(infoBytes)
	expected := strings.ToUpper(hex.EncodeToString(h[:]))

	got, err := feed.ComputeHashFromTorrentBytes(full)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestComputeHashFromTorrentBytes_Malformed(t *testing.T) {
	_, err := feed.ComputeHashFromTorrentBytes([]byte("not bencode"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metainfo load:")
}
