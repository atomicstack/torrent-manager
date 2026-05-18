package aiclassify

import (
	"context"
	"net/http"
	"net/http/httptest"
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
