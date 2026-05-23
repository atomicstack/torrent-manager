package aiclassify_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/atomicstack/torrent-manager/internal/aiclassify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// okPickDirResponse returns a valid Anthropic Messages API JSON response body
// with the given text as the single content block.
func okPickDirResponse(text string) string {
	return `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-4-7",
		"content": [{"type": "text", "text": ` + `"` + text + `"` + `}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 50, "output_tokens": 15}
	}`
}

func newPickDirTestClient(t *testing.T, handler http.Handler) *aiclassify.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return aiclassify.New("test-key", option.WithBaseURL(srv.URL))
}

// singleEpisodeContents is the canonical contents shape for a single-episode
// release — one file, no wrapper dir. Used to feed PickDir's mock tests
// without a real .torrent payload.
func singleEpisodeContents(filename string) aiclassify.TorrentContents {
	return aiclassify.TorrentContents{
		Name:       filename,
		SingleFile: true,
		Files:      []string{filename},
	}
}

func TestPickDir_Live(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live test")
	}
	c := aiclassify.New(key)
	res, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p.WEB-DL.AAC2.0.H.264-NTb.mkv",
		[]string{"beef", "fallout.2024", "severance", "the.amazing.race"},
		singleEpisodeContents("Severance.S02E01.1080p.WEB-DL.AAC2.0.H.264-NTb.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "severance", res.ShowDir, "should match existing dir exactly")
	assert.Equal(t, "season2", res.Subdir, "single-episode goes under season2")
	assert.False(t, res.IsNewDir)
}

func TestPickDir_NewShow_Live(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live test")
	}
	c := aiclassify.New(key)
	res, err := c.PickDir(context.Background(),
		"The.Bear.S03E04.1080p.HULU.WEB-DL.DDP5.1.H.264-NTb.mkv",
		[]string{"beef", "fallout.2024", "severance"},
		singleEpisodeContents("The.Bear.S03E04.1080p.HULU.WEB-DL.DDP5.1.H.264-NTb.mkv"))
	require.NoError(t, err)
	assert.True(t, res.IsNewDir, "should be a new dir")
	assert.NotEmpty(t, res.ShowDir)
	assert.Contains(t, res.ShowDir, "bear")
	assert.Equal(t, "season3", res.Subdir)
}

// TestPickDir_SeasonPack_Live exercises the new layout-aware subdir choice:
// a torrent whose wrapper directory IS a season folder should land directly
// under the show dir (subdir = "") so the wrapper itself becomes the season.
func TestPickDir_SeasonPack_Live(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live test")
	}
	c := aiclassify.New(key)
	res, err := c.PickDir(context.Background(),
		"Severance.S02.1080p.WEB-DL.AAC2.0.H.264-NTb",
		[]string{"beef", "severance"},
		aiclassify.TorrentContents{
			Name:       "Severance.S02.1080p.WEB-DL.AAC2.0.H.264-NTb",
			SingleFile: false,
			Files: []string{
				"Severance.S02E01.mkv",
				"Severance.S02E02.mkv",
				"Severance.S02E03.mkv",
			},
		})
	require.NoError(t, err)
	assert.Equal(t, "severance", res.ShowDir)
	assert.Equal(t, "", res.Subdir, "wrapper dir is already a season folder")
	assert.False(t, res.IsNewDir)
}

// TestPickDir_OverridesIsNewDir verifies the post-parse validation overrides
// the model's is_new_dir flag based on actual existingDirs membership. Even if
// the model claims is_new_dir=true for a show_dir that's actually in the list,
// we correct it to false (and vice versa).
func TestPickDir_OverridesIsNewDir(t *testing.T) {
	// model claims is_new_dir=true but show_dir is actually in existingDirs:
	// the override should set is_new_dir=false.
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"severance\", \"subdir\": \"season2\", \"is_new_dir\": true}`)))
	}))

	res, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p",
		[]string{"beef", "severance", "fallout.2024"},
		singleEpisodeContents("Severance.S02E01.1080p.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "severance", res.ShowDir)
	assert.Equal(t, "season2", res.Subdir)
	assert.False(t, res.IsNewDir, "override should correct is_new_dir to false when show_dir is in existingDirs")
}

func TestPickDir_OverridesIsNewDir_NewShow(t *testing.T) {
	// model claims is_new_dir=false but show_dir is NOT in existingDirs:
	// the override should set is_new_dir=true.
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"the.bear\", \"subdir\": \"season3\", \"is_new_dir\": false}`)))
	}))

	res, err := c.PickDir(context.Background(),
		"The.Bear.S03E04.1080p",
		[]string{"beef", "severance", "fallout.2024"},
		singleEpisodeContents("The.Bear.S03E04.1080p.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "the.bear", res.ShowDir)
	assert.Equal(t, "season3", res.Subdir)
	assert.True(t, res.IsNewDir, "override should correct is_new_dir to true when show_dir is not in existingDirs")
}

// TestPickDir_EmptySubdir verifies the model can return "" for subdir
// without tripping any post-parse validation — the empty value is the
// signal that the torrent already carries its own season folder.
func TestPickDir_EmptySubdir(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"severance\", \"subdir\": \"\", \"is_new_dir\": false}`)))
	}))

	res, err := c.PickDir(context.Background(),
		"Severance.S02.1080p",
		[]string{"severance"},
		aiclassify.TorrentContents{
			Name:       "Severance.S02.1080p",
			SingleFile: false,
			Files:      []string{"Severance.S02E01.mkv", "Severance.S02E02.mkv"},
		})
	require.NoError(t, err)
	assert.Equal(t, "severance", res.ShowDir)
	assert.Equal(t, "", res.Subdir)
}

func TestPickDir_EmptyShowDir(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"\", \"subdir\": \"\", \"is_new_dir\": false}`)))
	}))

	_, err := c.PickDir(context.Background(),
		"Garbage.Title", []string{"beef"},
		singleEpisodeContents("Garbage.Title.mkv"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty show_dir")
}

func TestPickDir_APIError(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`))
	}))

	_, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p", []string{"severance"},
		singleEpisodeContents("Severance.S02E01.1080p.mkv"))
	require.Error(t, err)
}

func TestPickDir_MalformedJSON(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`not-valid-json`)))
	}))

	_, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p", []string{"severance"},
		singleEpisodeContents("Severance.S02E01.1080p.mkv"))
	require.Error(t, err)
}

// TestPickDir_SanitisesProposedDirName guards against the regression where
// Claude appended stray punctuation ("pop.culture.jeopardy,") to a proposed
// new dir name. The package strips anything outside [a-z0-9.] before handing
// the value back to the caller.
func TestPickDir_SanitisesProposedDirName(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"pop.culture.jeopardy,\", \"subdir\": \"season2\", \"is_new_dir\": true}`)))
	}))
	res, err := c.PickDir(context.Background(),
		"Pop.Culture.Jeopardy.S02E10.1080p.WEB.h264-EDITH.mkv",
		[]string{"beef", "severance"},
		singleEpisodeContents("Pop.Culture.Jeopardy.S02E10.1080p.WEB.h264-EDITH.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "pop.culture.jeopardy", res.ShowDir, "trailing comma stripped")
	assert.Equal(t, "season2", res.Subdir)
	assert.True(t, res.IsNewDir)
}

// TestPickDir_PreservesHyphensInProposedDirName covers the relaxed character
// set for proposed show_dir names: dashes are kept when they sit inside a
// hyphenated word (the model is told to use them only for cases like
// "x-men" or "spider-man"). Stray punctuation around the dashes is still
// dropped.
func TestPickDir_PreservesHyphensInProposedDirName(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"x-men.97\", \"subdir\": \"season1\", \"is_new_dir\": true}`)))
	}))
	res, err := c.PickDir(context.Background(),
		"X-Men.97.S01E01.1080p", []string{"beef"},
		singleEpisodeContents("X-Men.97.S01E01.1080p.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "x-men.97", res.ShowDir, "internal dash preserved")
	assert.True(t, res.IsNewDir)
}

// TestPickDir_StripsSubdirDashes confirms that even though show_dir can keep
// dashes, season subdirectories never can — "season-3" collapses to
// "season.3" rather than passing through.
func TestPickDir_StripsSubdirDashes(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"severance\", \"subdir\": \"season-2\", \"is_new_dir\": false}`)))
	}))
	res, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p", []string{"severance"},
		singleEpisodeContents("Severance.S02E01.1080p.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "season.2", res.Subdir, "stray dash collapses to dot in subdir")
}

// TestPickDir_DoesNotSanitiseExistingDirNames ensures we trust an exact match
// from the existingDirs list as-is. The list itself is canonical (it's the
// on-disk filesystem listing) so we shouldn't second-guess it even if it
// contains characters our sanitiser would strip.
func TestPickDir_DoesNotSanitiseExistingDirNames(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"Guy Mont Spelling Bee\", \"subdir\": \"season1\", \"is_new_dir\": false}`)))
	}))
	res, err := c.PickDir(context.Background(),
		"Guy Montgomery's Guy Mont-Spelling Bee S01E01",
		[]string{"Guy Mont Spelling Bee", "severance"},
		singleEpisodeContents("Guy Montgomery's Guy Mont-Spelling Bee S01E01.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "Guy Mont Spelling Bee", res.ShowDir, "exact match preserved verbatim")
	assert.False(t, res.IsNewDir)
}
