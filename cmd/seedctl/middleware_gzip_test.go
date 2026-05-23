package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithGzip_DecompressesRequestBody confirms a gzip-encoded request body is
// transparently decompressed before reaching the next handler.
func TestWithGzip_DecompressesRequestBody(t *testing.T) {
	var seen []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		seen = b
		w.WriteHeader(http.StatusOK)
	})

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"hello":"world"}`))
	require.NoError(t, zw.Close())

	req := httptest.NewRequest("POST", "/test", &buf)
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	withGzip(next).ServeHTTP(rr, req)

	assert.Equal(t, `{"hello":"world"}`, string(seen))
	assert.Equal(t, http.StatusOK, rr.Code)
}

// TestWithGzip_CompressesResponseBody confirms the response is gzip-encoded
// when the client advertises Accept-Encoding: gzip, and Content-Encoding is set.
func TestWithGzip_CompressesResponseBody(t *testing.T) {
	plaintext := strings.Repeat(`{"feed_name":"TrackerA (TV)","title":"X"},`, 50)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(plaintext))
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	withGzip(next).ServeHTTP(rr, req)

	require.Equal(t, "gzip", rr.Header().Get("Content-Encoding"))
	require.Less(t, rr.Body.Len(), len(plaintext)/2, "compressed should be at least half the size")

	zr, err := gzip.NewReader(rr.Body)
	require.NoError(t, err)
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, plaintext, string(decoded))
}

// TestWithGzip_PassthroughWithoutHeaders confirms the middleware is a no-op
// when neither Content-Encoding nor Accept-Encoding is set.
func TestWithGzip_PassthroughWithoutHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain")
	})
	req := httptest.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()
	withGzip(next).ServeHTTP(rr, req)
	assert.Equal(t, "", rr.Header().Get("Content-Encoding"))
	assert.Equal(t, "plain", rr.Body.String())
}

// TestWithGzip_BadGzipBodyRejects confirms a Content-Encoding: gzip request
// with a non-gzip body returns 400 instead of hanging or panicking.
func TestWithGzip_BadGzipBodyRejects(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached when gzip body is malformed")
	})
	req := httptest.NewRequest("POST", "/test", strings.NewReader("not gzip"))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	withGzip(next).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
