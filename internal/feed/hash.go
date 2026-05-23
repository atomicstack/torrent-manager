package feed

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

var hashRegex = regexp.MustCompile(`(?i)\b[a-f0-9]{40}\b`)

// ExtractHash scans the item's guid + link for a 40-char hex info hash.
// Returns uppercase hash + true on the first match, or "" + false if none of
// the fields contain a recognisable hash.
func ExtractHash(it Item) (string, bool) {
	for _, s := range []string{it.GUID, it.Link} {
		if m := hashRegex.FindString(s); m != "" {
			return strings.ToUpper(m), true
		}
	}
	return "", false
}

// ComputeHashFromTorrentBytes parses bencoded .torrent bytes and returns the
// sha1 of the info dict (the info hash) as an uppercase hex string.
func ComputeHashFromTorrentBytes(b []byte) (string, error) {
	mi, err := metainfo.Load(bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("metainfo load: %w", err)
	}
	h := sha1.Sum(mi.InfoBytes)
	return strings.ToUpper(hex.EncodeToString(h[:])), nil
}
