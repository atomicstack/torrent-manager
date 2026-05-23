// Package torrent inspects bencoded .torrent payloads for human-friendly
// metadata: total bytes, file count, and the top-level name(s) that will
// land on disk.
package torrent

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// FileEntry is a single file inside the torrent.
//
// For multi-file torrents Path is relative to info.name (the wrapper dir),
// joined with "/". For single-file torrents Path equals info.name (since
// there is no wrapper dir — the file itself is the top-level entry).
type FileEntry struct {
	Path   string
	Length int64
}

// Metadata is the inspection result.
type Metadata struct {
	Name       string      // info.name — top-level filename (single-file) or root dir (multi-file)
	TotalBytes int64       // sum of all file lengths
	FileCount  int         // 1 for single-file torrents, len(info.files) otherwise
	Roots      []string    // disk entries the torrent will create. Always [Name] per BEP3.
	SingleFile bool        // true when the torrent contains a single file (no wrapper dir)
	Files      []FileEntry // every file in the torrent, relative paths joined with "/"
}

// Parse decodes a .torrent payload and returns a Metadata summary. The
// returned Roots slice always contains a single element (info.name); see the
// package doc for the rationale.
func Parse(b []byte) (Metadata, error) {
	mi, err := metainfo.Load(bytes.NewReader(b))
	if err != nil {
		return Metadata{}, fmt.Errorf("metainfo load: %w", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return Metadata{}, fmt.Errorf("unmarshal info: %w", err)
	}
	m := Metadata{
		Name:  info.Name,
		Roots: []string{info.Name},
	}
	if len(info.Files) == 0 {
		m.FileCount = 1
		m.TotalBytes = info.Length
		m.SingleFile = true
		m.Files = []FileEntry{{Path: info.Name, Length: info.Length}}
	} else {
		m.FileCount = len(info.Files)
		m.Files = make([]FileEntry, 0, len(info.Files))
		for _, f := range info.Files {
			m.TotalBytes += f.Length
			m.Files = append(m.Files, FileEntry{
				Path:   strings.Join(f.Path, "/"),
				Length: f.Length,
			})
		}
	}
	return m, nil
}

// HumanBytes formats a byte count using binary (1024) units.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), suffix)
}
