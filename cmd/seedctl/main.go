package main

import (
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/itemsdb"
	"github.com/atomicstack/torrent-manager/internal/rtorrent"
	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"gopkg.in/yaml.v3"
)

type config struct {
	Listen  string `yaml:"listen"`   // e.g. "0.0.0.0:18443"
	SCGI    string `yaml:"scgi"`     // e.g. "127.0.0.1:5059"
	ItemsDB string `yaml:"items_db"` // sqlite path; defaults to /home/seedbox/etc/seedctl/items.db
	// Allowlist accepts CIDRs ("10.0.0.0/24", "2001:db8::/64") or bare IPs
	// (treated as /32 v4 or /128 v6). Empty/missing list = no filter
	// (current behaviour). Connections from non-matching peers are dropped
	// after TCP accept but before TLS reads any bytes, so the noisy
	// "first record does not look like a TLS handshake" scans go silent.
	Allowlist []string `yaml:"allowlist,omitempty"`
	TLS       struct {
		CAFile   string `yaml:"ca_file"`
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
}

func main() {
	cfgPath := flag.String("config", "/etc/seedctl/seedctl.yaml", "config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if cfg.ItemsDB == "" {
		cfg.ItemsDB = "/home/seedbox/etc/seedctl/items.db"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	tlsCfg, err := tlsx.ServerConfig(cfg.TLS.CAFile, cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		logger.Error("load tls", "err", err)
		os.Exit(1)
	}

	client := rtorrent.NewClient(cfg.SCGI)
	items, err := itemsdb.Open(cfg.ItemsDB)
	if err != nil {
		logger.Error("open items db", "path", cfg.ItemsDB, "err", err)
		os.Exit(1)
	}

	restartCh := make(chan struct{}, 1)
	srv := &http.Server{
		Addr: cfg.Listen,
		// withLogging wraps the outside so req_bytes/resp_bytes count actual
		// wire bytes (post-gzip). withGzip decompresses request bodies sent
		// with Content-Encoding: gzip and compresses responses when the client
		// advertises Accept-Encoding: gzip.
		Handler:           withLogging(withGzip(newHandler(client, items, restartCh))),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	allowPrefixes, err := parseAllowlist(cfg.Allowlist)
	if err != nil {
		logger.Error("parse allowlist", "err", err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("listen", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}
	wrappedLn := newAllowlistListener(ln, allowPrefixes)

	go func() {
		logger.Info("listening",
			"addr", cfg.Listen,
			"scgi", cfg.SCGI,
			"allowlist_entries", len(allowPrefixes))
		if err := srv.ServeTLS(wrappedLn, "", ""); err != nil && err != http.ErrServerClosed {
			logger.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	shouldRestart := false
	select {
	case <-ctx.Done():
	case <-restartCh:
		shouldRestart = true
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	_ = items.Close()

	if shouldRestart {
		exe, err := os.Executable()
		if err != nil {
			logger.Error("restart_executable", "err", err)
			os.Exit(1)
		}
		logger.Info("restarting_via_exec", "exec", exe, "args", os.Args)
		if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
			logger.Error("restart_exec", "err", err)
			os.Exit(1)
		}
	}
}

func loadConfig(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, nil
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cn := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		// wrap body + response so we can count actual bytes across the wire.
		// req_bytes tracks bytes the handler consumed; resp_bytes tracks bytes
		// the handler wrote (excludes headers).
		bodyCounter := &countingReadCloser{ReadCloser: r.Body}
		r.Body = bodyCounter
		rw := &respRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"cn", cn,
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

type countingReadCloser struct {
	io.ReadCloser
	n int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// withGzip handles HTTP-layer gzip in both directions. JSON bodies on these
// endpoints typically shrink 6-10x; the cost is a sub-millisecond CPU pass.
//
// Request side: if Content-Encoding: gzip, replace r.Body with a gzip reader
// so the handler reads decompressed bytes. Strip the Content-Encoding +
// Content-Length headers so handlers don't see stale values.
//
// Response side: if Accept-Encoding includes gzip, wrap the ResponseWriter so
// the handler's writes are gzipped before reaching the underlying writer.
// Set Content-Encoding: gzip and clear Content-Length on the first write
// (or first WriteHeader, whichever comes first).
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "invalid gzip body", http.StatusBadRequest)
				return
			}
			defer zr.Close()
			r.Body = io.NopCloser(zr)
			r.Header.Del("Content-Encoding")
			r.Header.Del("Content-Length")
			r.ContentLength = -1
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		defer gz.Close()
		gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		next.ServeHTTP(gw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) writeHeaderOnce(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.ResponseWriter.Header()
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length") // compressed length unknown until Close()
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.writeHeaderOnce(status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.writeHeaderOnce(http.StatusOK)
	}
	return g.gz.Write(b)
}
