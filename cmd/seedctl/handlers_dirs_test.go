package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/atomicstack/torrent-manager/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDirsHandler wires the production handler against a fresh roots map
// pointing into t.TempDir(). The rtorrent stub + items db are no-ops because
// the /v1/dirs handler doesn't touch them.
func newTestDirsHandler(t *testing.T, dirsByRoot map[string][]string) http.Handler {
	t.Helper()
	roots := map[string]string{}
	for name, dirs := range dirsByRoot {
		base := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.MkdirAll(base, 0o755))
		for _, d := range dirs {
			require.NoError(t, os.MkdirAll(filepath.Join(base, d), 0o755))
		}
		roots[name] = base
	}
	return newHandler(&stubClient{}, nil, nil, roots)
}

func TestListDirs_ReturnsSortedNames(t *testing.T) {
	h := newTestDirsHandler(t, map[string][]string{
		"tv": {"severance", "fallout", "beef"},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv", nil))
	require.Equal(t, 200, rr.Code)
	var resp api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, []string{"beef", "fallout", "severance"}, resp.Dirs)
}

func TestListDirs_SkipsFilesAndHiddenDirs(t *testing.T) {
	base := filepath.Join(t.TempDir(), "tv")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "good"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(base, ".hidden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "a-file.txt"), []byte("x"), 0o644))

	h := newHandler(&stubClient{}, nil, nil, map[string]string{"tv": base})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv", nil))
	require.Equal(t, 200, rr.Code)
	var resp api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, []string{"good"}, resp.Dirs)
}

func TestListDirs_UnknownRootIs404(t *testing.T) {
	h := newHandler(&stubClient{}, nil, nil, map[string]string{"tv": "/tmp/nope"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=movies", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListDirs_RootWithoutOnDiskPath(t *testing.T) {
	// configured root, but the path doesn't exist on disk: return empty list,
	// not 500 — lets the caller proceed as if the root is fresh.
	h := newHandler(&stubClient{}, nil, nil, map[string]string{"tv": "/tmp/definitely-not-there-seedctl-test"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv", nil))
	require.Equal(t, 200, rr.Code)
	var resp api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Empty(t, resp.Dirs)
}

func TestListDirs_EmptyRootMapIs404(t *testing.T) {
	h := newHandler(&stubClient{}, nil, nil, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListDirs_EmptyRootValueIs404(t *testing.T) {
	// a key configured to the empty string shouldn't be treated as a valid root
	// (defensive — avoids accidentally listing the process cwd).
	h := newHandler(&stubClient{}, nil, nil, map[string]string{"tv": ""})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListDirs_EmptyDirReturnsEmptyList(t *testing.T) {
	h := newTestDirsHandler(t, map[string][]string{"tv": {}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv", nil))
	require.Equal(t, 200, rr.Code)
	// snapshot the wire payload before decoding (json.Decoder drains the body).
	// dirs must serialise as [] rather than null so JSON clients don't have to
	// handle a missing field.
	body := rr.Body.String()
	assert.Contains(t, body, `"dirs":[]`)
	var resp api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Empty(t, resp.Dirs)
}

// TestListDirs_SubListsNestedLevel covers the deep-completion path: sub= names
// a relative path under the root and the handler lists that dir's children.
func TestListDirs_SubListsNestedLevel(t *testing.T) {
	h := newTestDirsHandler(t, map[string][]string{
		"tv": {"only.connect/season2", "only.connect/season1", "beef"},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv&sub=only.connect", nil))
	require.Equal(t, 200, rr.Code)
	var resp api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, []string{"season1", "season2"}, resp.Dirs)
}

func TestListDirs_SubMissingIsEmpty(t *testing.T) {
	h := newTestDirsHandler(t, map[string][]string{"tv": {"beef"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv&sub=nope/deeper", nil))
	require.Equal(t, 200, rr.Code)
	var resp api.DirsListResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Empty(t, resp.Dirs)
}

// TestListDirs_SubRejectsEscapes makes sure sub= can't climb out of the root
// or reach hidden dirs.
func TestListDirs_SubRejectsEscapes(t *testing.T) {
	h := newTestDirsHandler(t, map[string][]string{"tv": {"beef", ".hidden/x"}})
	for _, sub := range []string{"..", "../..", "beef/../..", "/etc", ".hidden", "beef//x", "./beef"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dirs?root=tv&sub="+url.QueryEscape(sub), nil))
		assert.Equal(t, http.StatusBadRequest, rr.Code, "sub=%q", sub)
	}
}
