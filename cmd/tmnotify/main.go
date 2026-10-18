// tmnotify turns finished torrent-manager enqueues into mqtt messages. it
// holds seedctl's completion stream open over mtls (the seedbox can't reach
// the lan, so the lan side keeps the connection) and publishes each
// completion to the home assistant broker, acking it once published.
//
// run exactly one replica: two subscribers would both publish.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/atomicstack/torrent-manager/internal/notify"
	"github.com/atomicstack/torrent-manager/internal/tlsx"
	"github.com/atomicstack/torrent-manager/internal/watchlist"
)

// streamDownGrace is how long the seedctl stream may stay down (including
// right after startup) before /healthz reports unhealthy.
const streamDownGrace = 5 * time.Minute

func main() {
	seedctlURL := flag.String("seedctl-url", "https://seedbox.example.com:18443", "seedctl base url")
	watchPath := flag.String("watchlist", "/etc/tmgr/watchlist.yaml", "watchlist config (only roots are used, to shorten save_dir)")
	caPath := flag.String("ca", "/etc/tmgr/tls/ca.crt", "ca cert (mtls to seedctl)")
	certPath := flag.String("cert", "/etc/tmgr/tls/client.crt", "client cert (mtls)")
	keyPath := flag.String("key", "/etc/tmgr/tls/client.key", "client key (mtls)")
	healthAddr := flag.String("health-listen", ":8081", "listen address for /healthz")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	mcfg, err := mqttConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Error("mqtt config", "err", err)
		os.Exit(1)
	}
	if f, err := watchlist.Load(*watchPath); err != nil {
		logger.Warn("load watchlist; save_dir will be absolute", "err", err)
	} else {
		mcfg.DisplayRoot = displayRoot(f.Roots)
	}

	tlsCfg, err := tlsx.ClientConfig(*caPath, *certPath, *keyPath)
	if err != nil {
		logger.Error("load tls", "err", err)
		os.Exit(1)
	}
	// no overall client timeout: the stream is long-lived. the subscriber's
	// idle watchdog catches dead connections.
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig:       tlsCfg,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
	}}

	pub, err := notify.NewMQTTPublisher(mcfg)
	if err != nil {
		logger.Error("mqtt connect", "err", err)
		os.Exit(1)
	}
	defer pub.Close()
	logger.Info("mqtt connected",
		"url", mcfg.URL, "topic", mcfg.Topic, "user", mcfg.Username,
		"password_set", mcfg.Password != "", "display_root", mcfg.DisplayRoot)

	sub := &notify.Subscriber{
		BaseURL: *seedctlURL, Client: hc, Pub: pub,
		MinBackoff: time.Second, MaxBackoff: time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if healthy(pub.Connected(), sub.Connected(), sub.LastConnected(), started, time.Now()) {
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
	})
	hsrv := &http.Server{Addr: *healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := hsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health listen", "err", err)
			os.Exit(1)
		}
	}()

	_ = sub.Run(ctx)
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hsrv.Shutdown(shutCtx)
}

// mqttConfigFromEnv reads the broker settings. MQTT_URL is required: tmnotify
// has no other job, so a missing broker should fail loudly.
func mqttConfigFromEnv(getenv func(string) string) (notify.MQTTConfig, error) {
	cfg := notify.MQTTConfig{
		URL:      getenv("MQTT_URL"),
		Username: getenv("MQTT_USERNAME"),
		Password: getenv("MQTT_PASSWORD"),
		Topic:    getenv("MQTT_TOPIC"),
		ClientID: getenv("MQTT_CLIENT_ID"),
	}
	if cfg.URL == "" {
		return cfg, errors.New("MQTT_URL is not set")
	}
	if cfg.Topic == "" {
		cfg.Topic = "torrent-manager/complete"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "tmnotify"
	}
	return cfg, nil
}

// displayRoot is the parent shared by the tv and movies roots ("/t/" for
// /t/tv + /t/movies), mirroring tmweb's computeCommonRoot. empty when they
// don't share one.
func displayRoot(r watchlist.Roots) string {
	var parents []string
	for _, p := range []string{r.TV, r.Movies} {
		if p != "" {
			parents = append(parents, filepath.Dir(filepath.Clean(p)))
		}
	}
	if len(parents) == 0 {
		return ""
	}
	for _, p := range parents[1:] {
		if p != parents[0] {
			return ""
		}
	}
	return parents[0] + string(filepath.Separator)
}

// healthy: the broker must be connected, and the seedctl stream must be up or
// have been up (or the process started) within streamDownGrace.
func healthy(mqttUp, streamUp bool, lastUp, started, now time.Time) bool {
	if !mqttUp {
		return false
	}
	if streamUp {
		return true
	}
	ref := lastUp
	if ref.IsZero() {
		ref = started
	}
	return now.Sub(ref) < streamDownGrace
}
