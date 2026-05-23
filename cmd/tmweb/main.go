// Package main runs the tmweb http server: a lan-only web ui for browsing the
// rss items index in seedctl and (in later tasks) triggering manual enqueues.
//
// This binary is intentionally minimal in task 12: it parses flags, loads the
// watchlist for validation, opens an mtls http client pointed at seedctl, and
// serves a single /healthz endpoint. The search ui (task 13) and enqueue flow
// (task 14) build on top of this scaffolding.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/atomicstack/torrent-manager/internal/watchlist"
)

func main() {
	listenAddr := flag.String("listen", ":8080", "http listen address (lan-only, no tls)")
	seedctlURL := flag.String("seedctl-url", "https://seedbox.example.com:18443", "seedctl base url")
	watchPath := flag.String("watchlist", "/etc/tmgr/watchlist.yaml", "watchlist config")
	caPath := flag.String("ca", "/etc/tmgr/tls/ca.crt", "ca cert (mtls to seedctl)")
	certPath := flag.String("cert", "/etc/tmgr/tls/client.crt", "client cert (mtls)")
	keyPath := flag.String("key", "/etc/tmgr/tls/client.key", "client key (mtls)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// load + compile the watchlist up-front so a malformed config fails fast
	// rather than waiting for the first request that needs it. tmweb itself
	// doesn't yet act on the parsed rules; tasks 13/14 will.
	file, err := watchlist.Load(*watchPath)
	if err != nil {
		logger.Error("load watchlist", "err", err)
		os.Exit(1)
	}
	if _, _, err := watchlist.Compile(file); err != nil {
		logger.Error("compile watchlist", "err", err)
		os.Exit(1)
	}

	tlsCfg, err := tlsx.ClientConfig(*caPath, *certPath, *keyPath)
	if err != nil {
		logger.Error("tls", "err", err)
		os.Exit(1)
	}
	httpC := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	seedctl := NewSeedctl(*seedctlURL, httpC)

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           newMux(seedctl),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("listening", "addr", *listenAddr, "seedctl_url", *seedctlURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting_down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// newMux wires the http routes for tmweb. Extracted from main so tests can
// instantiate a handler against a stubbed *Seedctl.
func newMux(s *Seedctl) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// 5s timeout so a wedged seedctl can't hang a k8s liveness probe.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		ok, err := s.Healthz(ctx)
		if err != nil {
			slog.Debug("healthz_seedctl_ping_failed", "err", err)
		}
		// always 200: kubernetes liveness probes care about the body, and we
		// want the probe to read the json so it can distinguish "tmweb up,
		// seedctl down" from "tmweb hung".
		writeJSON(w, http.StatusOK, healthzResponse{
			OK:        ok,
			SeedctlOK: ok,
		})
	})
	return mux
}

type healthzResponse struct {
	OK        bool `json:"ok"`
	SeedctlOK bool `json:"seedctl_ok"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
