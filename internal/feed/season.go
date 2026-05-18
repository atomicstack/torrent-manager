package feed

import (
	"regexp"
	"strconv"
)

var seasonRegex = regexp.MustCompile(`(?i)\bS(\d+)E\d+\b`)

// ExtractSeason returns the season number from a release title via the standard
// SxxExx pattern. Returns (0, false) if no season+episode marker is present.
func ExtractSeason(title string) (int, bool) {
	m := seasonRegex.FindStringSubmatch(title)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
