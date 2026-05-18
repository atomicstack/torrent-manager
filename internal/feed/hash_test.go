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

func TestExtractHash_FromEnclosure(t *testing.T) {
	it := feed.Item{
		Enclosure: "https://tracker/dl/feedbeef1234567890abcdef1234567890abcdef.torrent",
	}
	h, ok := feed.ExtractHash(it)
	assert.True(t, ok)
	assert.Equal(t, "FEEDBEEF1234567890ABCDEF1234567890ABCDEF", h)
}

func TestExtractHash_NoMatch(t *testing.T) {
	it := feed.Item{GUID: "no-hex-anywhere", Link: "also nope", Enclosure: "or here"}
	_, ok := feed.ExtractHash(it)
	assert.False(t, ok)
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
