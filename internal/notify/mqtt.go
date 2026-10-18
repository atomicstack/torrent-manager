package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/atomicstack/torrent-manager/internal/api"
)

// Message is the json payload published per completion.
type Message struct {
	InfoHash   string `json:"info_hash"`
	Name       string `json:"name"`
	Title      string `json:"title,omitempty"`
	SaveDir    string `json:"save_dir"`
	FinishedAt int64  `json:"finished_at"`
}

// MessageFor builds the payload for c. save_dir is shown relative to
// displayRoot (the common tv/movies parent, like tmweb's modals) when it sits
// under it, and left absolute otherwise.
func MessageFor(c api.Completion, displayRoot string) ([]byte, error) {
	dir := c.SaveDir
	if displayRoot != "" {
		root := strings.TrimSuffix(displayRoot, "/") + "/"
		if rel, ok := strings.CutPrefix(dir, root); ok {
			dir = rel
		}
	}
	return json.Marshal(Message{
		InfoHash: c.InfoHash, Name: c.Name, Title: c.Title, SaveDir: dir, FinishedAt: c.FinishedAt,
	})
}

// MQTTConfig configures NewMQTTPublisher.
type MQTTConfig struct {
	URL         string // tcp://host:1883
	Username    string
	Password    string
	ClientID    string
	Topic       string
	DisplayRoot string
}

// MQTTPublisher publishes completions at qos 1, not retained.
type MQTTPublisher struct {
	client mqtt.Client
	topic  string
	root   string
}

// NewMQTTPublisher connects to the broker, failing if the first connect does
// not succeed (so a misconfigured deploy crashloops visibly). later drops
// reconnect automatically.
func NewMQTTPublisher(cfg MQTTConfig) (*MQTTPublisher, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.URL).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetAutoReconnect(true).
		SetConnectTimeout(10 * time.Second).
		SetKeepAlive(30 * time.Second)
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, fmt.Errorf("mqtt connect %s: timed out", cfg.URL)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt connect %s: %w", cfg.URL, err)
	}
	return &MQTTPublisher{client: c, topic: cfg.Topic, root: cfg.DisplayRoot}, nil
}

// Connected reports whether the broker connection is up.
func (p *MQTTPublisher) Connected() bool { return p.client.IsConnectionOpen() }

func (p *MQTTPublisher) Publish(ctx context.Context, c api.Completion) error {
	if !p.client.IsConnectionOpen() {
		return fmt.Errorf("mqtt: not connected")
	}
	payload, err := MessageFor(c, p.root)
	if err != nil {
		return err
	}
	tok := p.client.Publish(p.topic, 1, false, payload)
	select {
	case <-tok.Done():
		return tok.Error()
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("mqtt publish: timed out")
	}
}

// Close disconnects, giving in-flight publishes a moment to finish.
func (p *MQTTPublisher) Close() { p.client.Disconnect(500) }
