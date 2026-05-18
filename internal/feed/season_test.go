package feed

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractSeason(t *testing.T) {
	tests := []struct {
		title  string
		season int
		ok     bool
	}{
		{"Severance.S02E01.1080p.WEB-DL.DDP5.1.H.264-GROUP", 2, true},
		{"The.Boys.S04E08.AKA.Assassination.Run.2160p.AMZN.WEB-DL", 4, true},
		{"Some.Movie.2024.1080p.BluRay.x264-GROUP", 0, false},
		{"Show.s10e23.title", 10, true},
		{"", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.title, func(t *testing.T) {
			season, ok := ExtractSeason(tc.title)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.season, season)
		})
	}
}
