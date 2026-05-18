package torrent_test

import (
	"testing"

	"github.com/atomicstack/torrent-manager/internal/torrent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSingleFile crafts the minimum bencode for a single-file torrent.
//   d 8:announce 19:http://tracker.test
//     4:info d
//       6:length i<size>e
//       4:name <namelen>:<name>
//       12:piece length i16384e
//       6:pieces 0:
//     e
//   e
func buildSingleFile(name string, size int64) []byte {
	return []byte("d8:announce19:http://tracker.test4:infod" +
		"6:lengthi" + itoa(size) + "e" +
		"4:name" + slen(name) + ":" + name +
		"12:piece lengthi16384e" +
		"6:pieces0:" +
		"ee")
}

// buildMultiFile crafts a multi-file torrent. files is [(length, path...)].
func buildMultiFile(rootName string, files []struct {
	length int64
	path   []string
}) []byte {
	var inner string
	inner += "5:filesl"
	for _, f := range files {
		inner += "d"
		inner += "6:lengthi" + itoa(f.length) + "e"
		inner += "4:pathl"
		for _, p := range f.path {
			inner += slen(p) + ":" + p
		}
		inner += "e"
		inner += "e"
	}
	inner += "e"
	inner += "4:name" + slen(rootName) + ":" + rootName
	inner += "12:piece lengthi16384e"
	inner += "6:pieces0:"
	return []byte("d8:announce19:http://tracker.test4:infod" + inner + "ee")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}

func slen(s string) string {
	return itoa(int64(len(s)))
}

func TestParse_SingleFile(t *testing.T) {
	b := buildSingleFile("Bobs.Burgers.S16E14.1080p.WEB.h264-EDITH.mkv", 524_288_000) // 500 MiB
	md, err := torrent.Parse(b)
	require.NoError(t, err)
	assert.Equal(t, "Bobs.Burgers.S16E14.1080p.WEB.h264-EDITH.mkv", md.Name)
	assert.Equal(t, 1, md.FileCount)
	assert.Equal(t, int64(524_288_000), md.TotalBytes)
	assert.Equal(t, []string{"Bobs.Burgers.S16E14.1080p.WEB.h264-EDITH.mkv"}, md.Roots)
}

func TestParse_MultiFile(t *testing.T) {
	b := buildMultiFile("MovieName.2024.1080p.BluRay.x264-GROUP", []struct {
		length int64
		path   []string
	}{
		{length: 8_589_934_592, path: []string{"movie.mkv"}},                  // 8 GiB
		{length: 200_000, path: []string{"movie.nfo"}},                        // 200 KB
		{length: 500_000_000, path: []string{"extras", "behind_the_scenes.mkv"}}, // 500 MB in subdir
	})
	md, err := torrent.Parse(b)
	require.NoError(t, err)
	assert.Equal(t, "MovieName.2024.1080p.BluRay.x264-GROUP", md.Name)
	assert.Equal(t, 3, md.FileCount)
	assert.Equal(t, int64(8_589_934_592+200_000+500_000_000), md.TotalBytes)
	assert.Equal(t, []string{"MovieName.2024.1080p.BluRay.x264-GROUP"}, md.Roots,
		"per BEP3 a multi-file torrent has exactly one root: info.name")
}

func TestParse_Malformed(t *testing.T) {
	_, err := torrent.Parse([]byte("not bencode at all"))
	require.Error(t, err)
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1_048_576, "1.00 MiB"},
		{524_288_000, "500.00 MiB"},
		{8_589_934_592, "8.00 GiB"},
		{1_099_511_627_776, "1.00 TiB"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, torrent.HumanBytes(c.in), "n=%d", c.in)
	}
}
