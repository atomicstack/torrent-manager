package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	TLS     struct {
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
	defer func() { _ = items.Close() }()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           withLogging(newHandler(client, items)),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("listening", "addr", cfg.Listen, "scgi", cfg.SCGI)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			logger.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
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
