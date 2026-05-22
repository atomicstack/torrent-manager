package hashcache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/hashcache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tmpCachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "seen.db")
}

func TestNew_MissingFileIsUninitialised(t *testing.T) {
	c, err := hashcache.New(tmpCachePath(t))
	require.NoError(t, err)
	assert.False(t, c.Initialised())
	assert.Equal(t, 0, c.Len())
}

func TestPopulateThenReopen(t *testing.T) {
	path := tmpCachePath(t)
	c, err := hashcache.New(path)
	require.NoError(t, err)
	entries := []hashcache.Entry{
		{Hash: "BBBB000000000000000000000000000000000000", Name: "Zeta.Series.S01E01.mkv"},
		{Hash: "AAAA000000000000000000000000000000000000", Name: "Alpha.Series.S01E01.mkv"},
	}
	require.NoError(t, c.Populate(entries))
	assert.True(t, c.Initialised())
	assert.Equal(t, 2, c.Len())
	for _, e := range entries {
		assert.True(t, c.HasHash(e.Hash))
		assert.True(t, c.HasName(e.Name))
	}
	require.NoError(t, c.Close())

	c2, err := hashcache.New(path)
	require.NoError(t, err)
	assert.True(t, c2.Initialised())
	assert.Equal(t, 2, c2.Len())
}

func TestAdd_PersistsAcrossReopen(t *testing.T) {
	path := tmpCachePath(t)
	c, err := hashcache.New(path)
	require.NoError(t, err)
	require.NoError(t, c.Add("AABBCCDDEEFF00112233445566778899AABBCCDD", "Foo.S01E01.mkv"))
	require.NoError(t, c.Add("1122334455667788990011223344556677889900", "Bar.S01E01.mkv"))
	require.NoError(t, c.Close())

	c2, err := hashcache.New(path)
	require.NoError(t, err)
	assert.Equal(t, 2, c2.Len())
	assert.True(t, c2.HasHash("AABBCCDDEEFF00112233445566778899AABBCCDD"))
	assert.True(t, c2.HasHash("aabbccddeeff00112233445566778899aabbccdd"), "hash lookup is case-insensitive")
	assert.True(t, c2.HasName("foo.s01e01.mkv"), "name lookup is case-insensitive")
}

func TestAdd_DuplicateHashIsNoOp(t *testing.T) {
	c, err := hashcache.New(tmpCachePath(t))
	require.NoError(t, err)
	require.NoError(t, c.Add("AABBCCDDEEFF00112233445566778899AABBCCDD", "Foo.mkv"))
	require.NoError(t, c.Add("AABBCCDDEEFF00112233445566778899AABBCCDD", "Foo.mkv"))
	assert.Equal(t, 1, c.Len())
}

func TestAdd_InvalidHashReturnsError(t *testing.T) {
	c, err := hashcache.New(tmpCachePath(t))
	require.NoError(t, err)
	err = c.Add("not-a-hash", "Foo.mkv")
	require.Error(t, err)
	assert.Equal(t, 0, c.Len())
}

func TestHasName_EmptyNameDoesNotMatch(t *testing.T) {
	c, err := hashcache.New(tmpCachePath(t))
	require.NoError(t, err)
	require.NoError(t, c.Add("AABBCCDDEEFF00112233445566778899AABBCCDD", ""))
	assert.False(t, c.HasName(""), "empty names must not register for name lookup")
	assert.True(t, c.HasHash("AABBCCDDEEFF00112233445566778899AABBCCDD"))
}

func TestFileSize_TracksDiskBytes(t *testing.T) {
	path := tmpCachePath(t)
	c, err := hashcache.New(path)
	require.NoError(t, err)
	// sqlite always materialises the file on open, so size is non-zero from
	// the start. confirm parity with os.Stat across an Add.
	require.NoError(t, c.Add("AABBCCDDEEFF00112233445566778899AABBCCDD", "Foo.mkv"))
	sz := c.FileSize()
	assert.Greater(t, sz, int64(0))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fi.Size(), sz)
}

func TestNew_NilReceiverIsNoOp(t *testing.T) {
	var c *hashcache.Cache
	assert.False(t, c.HasHash("AABBCCDDEEFF00112233445566778899AABBCCDD"))
	assert.False(t, c.HasName("anything"))
	assert.Equal(t, 0, c.Len())
	assert.False(t, c.Initialised())
	require.NoError(t, c.Add("AABBCCDDEEFF00112233445566778899AABBCCDD", "x"))
}
