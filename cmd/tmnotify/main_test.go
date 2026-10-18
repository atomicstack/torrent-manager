package main

import (
	"testing"
	"time"

	"github.com/atomicstack/torrent-manager/internal/watchlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestMQTTConfigFromEnv_RequiresURL(t *testing.T) {
	_, err := mqttConfigFromEnv(envMap(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MQTT_URL")
}

func TestMQTTConfigFromEnv_Defaults(t *testing.T) {
	cfg, err := mqttConfigFromEnv(envMap(map[string]string{
		"MQTT_URL": "tcp://10.0.0.1:1883", "MQTT_USERNAME": "u", "MQTT_PASSWORD": "p",
	}))
	require.NoError(t, err)
	assert.Equal(t, "tcp://10.0.0.1:1883", cfg.URL)
	assert.Equal(t, "u", cfg.Username)
	assert.Equal(t, "p", cfg.Password)
	assert.Equal(t, "torrent-manager/complete", cfg.Topic)
	assert.Equal(t, "tmnotify", cfg.ClientID)

	cfg, err = mqttConfigFromEnv(envMap(map[string]string{"MQTT_URL": "tcp://x:1883", "MQTT_TOPIC": "a/b"}))
	require.NoError(t, err)
	assert.Equal(t, "a/b", cfg.Topic)
}

func TestDisplayRoot(t *testing.T) {
	assert.Equal(t, "/t/", displayRoot(watchlist.Roots{TV: "/t/tv", Movies: "/t/movies"}))
	assert.Equal(t, "", displayRoot(watchlist.Roots{TV: "/a/tv", Movies: "/b/movies"}))
	assert.Equal(t, "/t/", displayRoot(watchlist.Roots{TV: "/t/tv"}))
	assert.Equal(t, "", displayRoot(watchlist.Roots{}))
}

func TestHealthy(t *testing.T) {
	now := time.Unix(10_000, 0)
	started := now.Add(-time.Hour)
	assert.True(t, healthy(true, true, time.Time{}, started, now), "both up")
	assert.False(t, healthy(false, true, time.Time{}, started, now), "mqtt down")
	assert.True(t, healthy(true, false, now.Add(-time.Minute), started, now), "stream reconnecting briefly")
	assert.False(t, healthy(true, false, now.Add(-10*time.Minute), started, now), "stream down too long")
	assert.True(t, healthy(true, false, time.Time{}, now.Add(-time.Minute), now), "startup grace")
	assert.False(t, healthy(true, false, time.Time{}, started, now), "never connected")
}
