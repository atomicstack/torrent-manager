// Package torrent inspects bencoded .torrent payloads for human-friendly
// metadata: total bytes, file count, and the top-level name(s) that will
// land on disk.
package torrent

import (
	"bytes"
	"fmt"

	"github.com/anacrolix/torrent/metainfo"
)

// Metadata is the inspection result.
type Metadata struct {
	Name       string   // info.name — top-level filename (single-file) or root dir (multi-file)
	TotalBytes int64    // sum of all file lengths
	FileCount  int      // 1 for single-file torrents, len(info.files) otherwise
	Roots      []string // disk entries the torrent will create. Always [Name] per BEP3.
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
	} else {
		m.FileCount = len(info.Files)
		for _, f := range info.Files {
			m.TotalBytes += f.Length
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
