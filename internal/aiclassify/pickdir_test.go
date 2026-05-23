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

func TestPickDir_Live(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live test")
	}
	c := aiclassify.New(key)
	res, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p.WEB-DL.AAC2.0.H.264-NTb.mkv",
		[]string{"beef", "fallout.2024", "severance", "the.amazing.race"})
	require.NoError(t, err)
	assert.Equal(t, "severance", res.ShowDir, "should match existing dir exactly")
	assert.Equal(t, 2, res.Season)
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
		[]string{"beef", "fallout.2024", "severance"})
	require.NoError(t, err)
	assert.True(t, res.IsNewDir, "should be a new dir")
	assert.NotEmpty(t, res.ShowDir)
	assert.Contains(t, res.ShowDir, "bear")
	assert.Equal(t, 3, res.Season)
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
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"severance\", \"season\": 2, \"is_new_dir\": true}`)))
	}))

	res, err := c.PickDir(context.Background(),
		"Severance.S02E01.1080p",
		[]string{"beef", "severance", "fallout.2024"})
	require.NoError(t, err)
	assert.Equal(t, "severance", res.ShowDir)
	assert.Equal(t, 2, res.Season)
	assert.False(t, res.IsNewDir, "override should correct is_new_dir to false when show_dir is in existingDirs")
}

func TestPickDir_OverridesIsNewDir_NewShow(t *testing.T) {
	// model claims is_new_dir=false but show_dir is NOT in existingDirs:
	// the override should set is_new_dir=true.
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"the.bear\", \"season\": 3, \"is_new_dir\": false}`)))
	}))

	res, err := c.PickDir(context.Background(),
		"The.Bear.S03E04.1080p",
		[]string{"beef", "severance", "fallout.2024"})
	require.NoError(t, err)
	assert.Equal(t, "the.bear", res.ShowDir)
	assert.Equal(t, 3, res.Season)
	assert.True(t, res.IsNewDir, "override should correct is_new_dir to true when show_dir is not in existingDirs")
}

func TestPickDir_EmptyShowDir(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"\", \"season\": 0, \"is_new_dir\": false}`)))
	}))

	_, err := c.PickDir(context.Background(), "Garbage.Title", []string{"beef"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty show_dir")
}

func TestPickDir_NegativeSeason(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"severance\", \"season\": -1, \"is_new_dir\": false}`)))
	}))

	_, err := c.PickDir(context.Background(), "Severance.SXX.1080p", []string{"severance"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative season")
}

func TestPickDir_APIError(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`))
	}))

	_, err := c.PickDir(context.Background(), "Severance.S02E01.1080p", []string{"severance"})
	require.Error(t, err)
}

func TestPickDir_MalformedJSON(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`not-valid-json`)))
	}))

	_, err := c.PickDir(context.Background(), "Severance.S02E01.1080p", []string{"severance"})
	require.Error(t, err)
}

// TestPickDir_SanitisesProposedDirName guards against the regression where
// Claude appended stray punctuation ("pop.culture.jeopardy,") to a proposed
// new dir name. The package strips anything outside [a-z0-9.] before handing
// the value back to the caller.
func TestPickDir_SanitisesProposedDirName(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"pop.culture.jeopardy,\", \"season\": 2, \"is_new_dir\": true}`)))
	}))
	res, err := c.PickDir(context.Background(),
		"Pop.Culture.Jeopardy.S02E10.1080p.WEB.h264-EDITH.mkv",
		[]string{"beef", "severance"})
	require.NoError(t, err)
	assert.Equal(t, "pop.culture.jeopardy", res.ShowDir, "trailing comma stripped")
	assert.Equal(t, 2, res.Season)
	assert.True(t, res.IsNewDir)
}

// TestPickDir_DoesNotSanitiseExistingDirNames ensures we trust an exact match
// from the existingDirs list as-is. The list itself is canonical (it's the
// on-disk filesystem listing) so we shouldn't second-guess it even if it
// contains characters our sanitiser would strip.
func TestPickDir_DoesNotSanitiseExistingDirNames(t *testing.T) {
	c := newPickDirTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okPickDirResponse(`{\"show_dir\": \"Guy Mont Spelling Bee\", \"season\": 1, \"is_new_dir\": false}`)))
	}))
	res, err := c.PickDir(context.Background(),
		"Guy Montgomery's Guy Mont-Spelling Bee S01E01",
		[]string{"Guy Mont Spelling Bee", "severance"})
	require.NoError(t, err)
	assert.Equal(t, "Guy Mont Spelling Bee", res.ShowDir, "exact match preserved verbatim")
	assert.False(t, res.IsNewDir)
}
