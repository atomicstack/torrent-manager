package main

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// noisyLowSignalPaths marks endpoints whose access-log spam outweighs the
// per-request value. Logged at DEBUG instead of INFO so they're still
// available if the operator turns up verbosity.
var noisyLowSignalPaths = map[string]bool{
	"/healthz": true,
}

// withLogging wraps a handler with structured access logging. One log line
// per request carries method, path, status, latency, and request/response
// byte counts. Static assets (/static/) and /healthz are demoted to DEBUG
// so the steady-state log stream stays useful.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		bodyCounter := &countingReadCloser{ReadCloser: r.Body}
		r.Body = bodyCounter
		rw := &respRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		level := slog.LevelInfo
		if noisyLowSignalPaths[r.URL.Path] {
			level = slog.LevelDebug
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rw.status,
			"remote", r.RemoteAddr,
			"req_bytes", bodyCounter.n,
			"resp_bytes", rw.bytes,
			"latency_ms", time.Since(start).Milliseconds())
	})
}

type respRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *respRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *respRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying ResponseWriter when it supports it.
// Required for SSE streams (enqueue-classify) and any other handler that
// needs to push partial data before returning.
func (s *respRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type countingReadCloser struct {
	io.ReadCloser
	n int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}
