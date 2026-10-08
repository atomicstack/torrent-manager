package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/atomicstack/torrent-manager/internal/savedir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dirTreeStub fakes seedctl's GET /v1/dirs over a fixed tree keyed on
// "<root>/<sub>" (sub empty for the root itself).
func dirTreeStub(t *testing.T, tree map[string][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dirs" {
			http.NotFound(w, r)
			return
		}
		key := r.URL.Query().Get("root")
		if sub := r.URL.Query().Get("sub"); sub != "" {
			key += "/" + sub
		}
		dirs := tree[key]
		if dirs == nil {
			dirs = []string{}
		}
		_ = json.NewEncoder(w).Encode(api.DirsListResponse{Dirs: dirs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getDirCompletions(t *testing.T, h http.Handler, saveDir string) []string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/dir-completions?save_dir="+url.QueryEscape(saveDir), nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var out api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&out))
	return out.Dirs
}

// TestDirCompletions_DeepLevels is the regression for completion stopping
// after the first level: typing past a second "/" must list that dir's
// children, not keep offering the root-level show dirs.
func TestDirCompletions_DeepLevels(t *testing.T) {
	srv := dirTreeStub(t, map[string][]string{
		"tv":                      {"beef", "only.connect"},
		"movies":                  {"heat"},
		"tv/only.connect":         {"season1", "season2"},
		"tv/only.connect/season2": {"extras"},
	})
	r, err := newRenderer()
	require.NoError(t, err)
	h := newMux(NewSeedctl(srv.URL, srv.Client()), nil, nil, nil, nil,
		savedir.Roots{TV: "/t/tv", Movies: "/t/movies"}, nil, r)

	assert.Equal(t, []string{"tv/only.connect/season1", "tv/only.connect/season2"},
		getDirCompletions(t, h, "tv/only.connect/"))
	assert.Equal(t, []string{"tv/only.connect/season1", "tv/only.connect/season2"},
		getDirCompletions(t, h, "tv/only.connect/sea"))
	assert.Equal(t, []string{"tv/only.connect/season2/extras"},
		getDirCompletions(t, h, "tv/only.connect/season2/"))
	assert.Equal(t, []string{"tv/beef", "tv/only.connect"},
		getDirCompletions(t, h, "tv/onl"))
	// no "/" yet: offer the first level under every root.
	assert.Equal(t, []string{"tv/beef", "tv/only.connect", "movies/heat"},
		getDirCompletions(t, h, "t"))
	assert.Empty(t, getDirCompletions(t, h, "nope/x/"))
	assert.Empty(t, getDirCompletions(t, h, "/abs/path/"))
}
