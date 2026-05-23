package aiclassify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// okResponse returns a valid Anthropic Messages API JSON response body with the
// given text as the single content block.
func okResponse(text string) string {
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

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New("test-key", option.WithBaseURL(srv.URL))
}

func TestClassify_Success(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResponse(`{\"show\": \"severance\", \"season\": 2}`)))
	}))

	res, err := c.Classify(context.Background(), "Severance.S02E01.1080p")
	require.NoError(t, err)
	assert.Equal(t, "severance", res.Show)
	assert.Equal(t, 2, res.Season)
}

func TestClassify_APIError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`))
	}))

	_, err := c.Classify(context.Background(), "Severance.S02E01.1080p")
	require.Error(t, err)
}

func TestClassify_MalformedJSON(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResponse(`not-valid-json`)))
	}))

	_, err := c.Classify(context.Background(), "Severance.S02E01.1080p")
	require.Error(t, err)
}

// TestClassify_LogsAnthropicUsage asserts the SDK round-trip emits a single
// structured "anthropic_call" log line with model + token counts so the
// operator can audit spend per call.
func TestClassify_LogsAnthropicUsage(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResponse(`{\"show\": \"x\", \"season\": 1}`)))
	}))
	_, err := c.Classify(context.Background(), "X.S01E01.1080p")
	require.NoError(t, err)

	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["msg"] == "anthropic_call" {
			found = ev
			break
		}
	}
	require.NotNil(t, found, "expected an anthropic_call log line")
	assert.Equal(t, "classify", found["call"])
	assert.Equal(t, float64(50), found["input_tokens"])
	assert.Equal(t, float64(15), found["output_tokens"])
	assert.NotEmpty(t, found["model"])
}

func TestClassify_EmptyResult(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResponse(`{\"show\": \"\", \"season\": 0}`)))
	}))

	res, err := c.Classify(context.Background(), "SomeMovie.2024.1080p")
	require.NoError(t, err)
	assert.Equal(t, "", res.Show)
	assert.Equal(t, 0, res.Season)
}
