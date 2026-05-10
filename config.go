package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML configuration.
type Config struct {
	Device       DeviceConfig  `yaml:"device"`
	Debug        bool          `yaml:"debug"`
	HTTP         HTTPConfig    `yaml:"http"`
	RemoteConfig string        `yaml:"remoteConfig"`
	Certificates []Certificate `yaml:"certificates"`
	Centers      []Center      `yaml:"centers"`
	Listen       []ListenEntry `yaml:"listen"`
	Targets      []TargetEntry `yaml:"targets"`
	Forward      []ForwardRule `yaml:"forward"`

	// Resolved lookup maps (populated by LoadConfig)
	certByID    map[string]*Certificate
	centerByID  map[string]*Center
	listenByID  map[string]*ListenEntry
	targetByID  map[string]*TargetEntry
}

type DeviceConfig struct {
	Name                     string `yaml:"name"`
	AllowedInterfacePatterns string `yaml:"allowedInterfacePatterns"`
	MaxRuntime               int    `yaml:"maxRuntime"` // seconds, default 3600
}

type HTTPConfig struct {
	Port string `yaml:"port"` // default ":9144"
}

type Certificate struct {
	ID   string `yaml:"id"`
	CA   string `yaml:"ca"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type Center struct {
	ID          string `yaml:"id"`
	URL         string `yaml:"url"`
	Token       string `yaml:"token"`
	TextMsgID   string `yaml:"textMsgId"`
	ImageMsgID  string `yaml:"imageMsgId"`
	Certificate string `yaml:"certificate"` // references Certificate.ID
	Encoding    string `yaml:"encoding"`    // "base64" (default) or "raw"
}

type ListenEntry struct {
	ID                string `yaml:"id"`
	DSN               string `yaml:"dsn"`
	AllowInterfaceIPs string `yaml:"allowInterfaceIps"`

	// Resolved at load time
	MQTTConfig     *MQTTConfig
	MaxMessageSize int64
	Certificate    *Certificate
}

type TargetEntry struct {
	ID                string `yaml:"id"`
	DSN               string `yaml:"dsn"`
	AllowInterfaceIPs string `yaml:"allowInterfaceIps"`

	// Resolved at load time
	IsMQTT         bool
	ParsedURL      *url.URL
	MQTTConfig     *MQTTConfig
	Username       string
	Password       string
	MaxMessageSize int64
	Certificate    *Certificate
	Types          []string
	Retries        int
	RetryDelay     int
}

type ForwardRule struct {
	From   []string `yaml:"from"`
	To     []string `yaml:"to"`
	Center string   `yaml:"center"` // optional, references Center.ID
}

var appConfig *Config

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		HTTP: HTTPConfig{Port: ":9144"},
	}
	cfg.Device.MaxRuntime = 3600

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Build lookup maps
	cfg.certByID = make(map[string]*Certificate)
	for i := range cfg.Certificates {
		c := &cfg.Certificates[i]
		if c.ID == "" {
			return nil, fmt.Errorf("certificate entry missing id")
		}
		cfg.certByID[c.ID] = c
	}

	cfg.centerByID = make(map[string]*Center)
	for i := range cfg.Centers {
		c := &cfg.Centers[i]
		if c.ID == "" {
			return nil, fmt.Errorf("center entry missing id")
		}
		if _, dup := cfg.centerByID[c.ID]; dup {
			return nil, fmt.Errorf("duplicate center id: %s", c.ID)
		}
		// Resolve certificate reference
		if c.Certificate != "" {
			if cfg.certByID[c.Certificate] == nil {
				return nil, fmt.Errorf("center %s references unknown certificate: %s", c.ID, c.Certificate)
			}
		}
		cfg.centerByID[c.ID] = c
	}

	cfg.listenByID = make(map[string]*ListenEntry)
	for i := range cfg.Listen {
		e := &cfg.Listen[i]
		if e.ID == "" {
			return nil, fmt.Errorf("listen entry missing id")
		}
		if isReservedID(e.ID) {
			return nil, fmt.Errorf("listen entry uses reserved id: %s", e.ID)
		}
		if _, dup := cfg.listenByID[e.ID]; dup {
			return nil, fmt.Errorf("duplicate listen id: %s", e.ID)
		}
		if err := resolveListenEntry(e, cfg.certByID); err != nil {
			return nil, fmt.Errorf("listen %s: %w", e.ID, err)
		}
		cfg.listenByID[e.ID] = e
	}

	cfg.targetByID = make(map[string]*TargetEntry)
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if t.ID == "" {
			return nil, fmt.Errorf("target entry missing id")
		}
		if isReservedID(t.ID) {
			return nil, fmt.Errorf("target entry uses reserved id: %s", t.ID)
		}
		if _, dup := cfg.targetByID[t.ID]; dup {
			return nil, fmt.Errorf("duplicate target id: %s", t.ID)
		}
		if err := resolveTargetEntry(t, cfg.certByID); err != nil {
			return nil, fmt.Errorf("target %s: %w", t.ID, err)
		}
		cfg.targetByID[t.ID] = t
	}

	// Validate device name
	if cfg.Device.Name == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("device.name is required and hostname unavailable")
		}
		cfg.Device.Name = hostname
		log.Printf("device.name not set, using hostname: %s", hostname)
	}

	// Validate forward rules
	for i, rule := range cfg.Forward {
		for _, id := range rule.From {
			if id != "system" && id != "http" && cfg.listenByID[id] == nil {
				return nil, fmt.Errorf("forward[%d].from references unknown id: %s", i, id)
			}
		}
		for _, id := range rule.To {
			if id != "system" && cfg.targetByID[id] == nil {
				return nil, fmt.Errorf("forward[%d].to references unknown id: %s", i, id)
			}
		}
		if rule.Center != "" && cfg.centerByID[rule.Center] == nil {
			return nil, fmt.Errorf("forward[%d].center references unknown center: %s", i, rule.Center)
		}
	}

	return cfg, nil
}

func (c *Config) GetCenterByID(id string) *Center {
	return c.centerByID[id]
}

func (c *Config) GetCertificateByID(id string) *Certificate {
	return c.certByID[id]
}

func (c *Config) GetListenByID(id string) *ListenEntry {
	return c.listenByID[id]
}

func (c *Config) GetTargetByID(id string) *TargetEntry {
	return c.targetByID[id]
}

func ConfigPath() string {
	if p := os.Getenv("CLIPBOARD_CONFIG_PATH"); p != "" {
		return p
	}
	return "config.yaml"
}

// isReservedID checks if an ID is a reserved keyword that cannot be used for
// user-defined entries (listen, target, center).
// Reserved IDs: "system" (local clipboard), "http" (HTTP receive endpoint).
func isReservedID(id string) bool {
	return id == "system" || id == "http"
}

func IsDebug() bool {
	if os.Getenv("CLIPBOARD_DEBUG") == "1" {
		return true
	}
	if appConfig != nil {
		return appConfig.Debug
	}
	return false
}

// resolveListenEntry parses the DSN and resolves certificate references.
func resolveListenEntry(e *ListenEntry, certMap map[string]*Certificate) error {
	u, err := url.Parse(e.DSN)
	if err != nil {
		return fmt.Errorf("invalid DSN: %w", err)
	}

	isMQTT := u.Scheme == "mqtt" || u.Scheme == "mqtts"
	if !isMQTT {
		return fmt.Errorf("listen entries only support MQTT DSNs, got scheme: %s", u.Scheme)
	}

	mqttCfg, maxSize, err := parseMQTTDSN(e.DSN)
	if err != nil {
		return err
	}
	e.MQTTConfig = mqttCfg
	e.MaxMessageSize = maxSize

	// Resolve certificate
	certID := u.Query().Get("certificate")
	if certID != "" {
		cert, ok := certMap[certID]
		if !ok {
			return fmt.Errorf("references unknown certificate: %s", certID)
		}
		e.Certificate = cert
	}

	return nil
}

// resolveTargetEntry parses the DSN and resolves references.
func resolveTargetEntry(t *TargetEntry, certMap map[string]*Certificate) error {
	u, err := url.Parse(t.DSN)
	if err != nil {
		return fmt.Errorf("invalid DSN: %w", err)
	}

	t.ParsedURL = u
	t.IsMQTT = u.Scheme == "mqtt" || u.Scheme == "mqtts"

	if t.IsMQTT {
		mqttCfg, maxSize, err := parseMQTTDSN(t.DSN)
		if err != nil {
			return err
		}
		t.MQTTConfig = mqttCfg
		t.MaxMessageSize = maxSize
	}

	if u.User != nil {
		t.Username = u.User.Username()
		t.Password, _ = u.User.Password()
	}

	q := u.Query()
	t.Types = parseCommaList(q.Get("types"))

	t.Retries = 0 // HTTP default
	if t.IsMQTT {
		t.Retries = 1
	}
	if v := q.Get("retries"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			t.Retries = n
		}
	}

	t.RetryDelay = 50
	if v := q.Get("retryDelay"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			t.RetryDelay = n
		}
	}

	// Resolve certificate
	certID := u.Query().Get("certificate")
	if certID != "" {
		cert, ok := certMap[certID]
		if !ok {
			return fmt.Errorf("references unknown certificate: %s", certID)
		}
		t.Certificate = cert
	}

	return nil
}

// parseMQTTDSN parses an MQTT DSN and returns config, maxMessageSize, error.
func parseMQTTDSN(rawURL string) (*MQTTConfig, int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid MQTT URL: %w", err)
	}

	if u.Scheme != "mqtt" && u.Scheme != "mqtts" {
		return nil, 0, fmt.Errorf("invalid scheme: %s, expected mqtt or mqtts", u.Scheme)
	}

	cfg := &MQTTConfig{
		Broker: u.Host,
		TLS:    u.Scheme == "mqtts",
		Topic:  strings.TrimPrefix(u.Path, "/"),
	}

	if u.User != nil {
		cfg.Username = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}

	q := u.Query()

	if v := q.Get("clientId"); v != "" {
		cfg.ClientID = v
	} else {
		cfg.ClientID = fmt.Sprintf("clipboard-sync-%s", hashURL(rawURL)[:8])
	}

	cfg.ConnectTimeout = 3
	if v := q.Get("connectTimeout"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.ConnectTimeout = int(t.Seconds())
		}
	}

	cfg.KeepAliveInterval = 60
	if v := q.Get("keepAliveInterval"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.KeepAliveInterval = int(t.Seconds())
		}
	}

	cfg.AutoReconnect = true
	if v := q.Get("automaticReconnect"); v != "" {
		cfg.AutoReconnect = v == "true"
	}

	cfg.ReconnectInterval = 5
	if v := q.Get("reconnectInterval"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.ReconnectInterval = int(t.Seconds())
		}
	}

	cfg.ReconnectMaxInterval = 60
	if v := q.Get("reconnectMaxInterval"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.ReconnectMaxInterval = int(t.Seconds())
		}
	}

	cfg.QoS = 1
	if v := q.Get("qos"); v == "0" {
		cfg.QoS = 0
	} else if v == "2" {
		cfg.QoS = 2
	}

	cfg.Retain = true
	if v := q.Get("retain"); v != "" {
		cfg.Retain = v == "true"
	}

	cfg.AllowInterfaceIps = q.Get("allowInterfaceIps")

	cfg.Retries = 1
	if v := q.Get("retries"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Retries = n
		}
	}

	cfg.RetryDelay = 50
	if v := q.Get("retryDelay"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RetryDelay = n
		}
	}

	// Parse maxMessageSize
	var maxSize int64
	if v := q.Get("maxMessageSize"); v != "" {
		maxSize, err = parseMaxMessageSize(v)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid maxMessageSize: %w", err)
		}
	}

	return cfg, maxSize, nil
}

// parseMaxMessageSize parses size strings like "5MB", "512KB", "1GB", or bare bytes.
func parseMaxMessageSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}

	multipliers := map[string]int64{
		"GB": 1 << 30,
		"MB": 1 << 20,
		"KB": 1 << 10,
	}

	for suffix, mult := range multipliers {
		if strings.HasSuffix(s, suffix) {
			numStr := strings.TrimSuffix(s, suffix)
			n, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size number: %s", numStr)
			}
			return int64(n * float64(mult)), nil
		}
	}

	// Bare number = bytes
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size: %s", s)
	}
	return n, nil
}

// ConnectionPoolKey returns a key for sharing MQTT connections by broker+credentials.
func ConnectionPoolKey(cfg *MQTTConfig) string {
	key := fmt.Sprintf("%s|%s|%s", cfg.Broker, cfg.Username, cfg.Password)
	return hashURL(key)
}

// parseCommaList splits a comma-separated string into a trimmed slice.
func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			result = append(result, v)
		}
	}
	return result
}

// GetBrokerAddress returns the broker address with scheme prefix for paho MQTT.
func (cfg *MQTTConfig) GetBrokerAddress() string {
	if cfg.TLS {
		return "ssl://" + cfg.Broker
	}
	return "tcp://" + cfg.Broker
}

// SubscribeTopics returns the topic with {$type} replaced by MQTT wildcard + for subscriptions.
func (cfg *MQTTConfig) SubscribeTopic() string {
	return strings.ReplaceAll(cfg.Topic, "{$type}", "+")
}

// BuildMQTTClientOptions creates paho MQTT client options from MQTTConfig with optional TLS.
func BuildMQTTClientOptions(cfg *MQTTConfig, cert *Certificate) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.GetBrokerAddress())
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	opts.SetConnectTimeout(time.Duration(cfg.ConnectTimeout) * time.Second)
	opts.SetKeepAlive(time.Duration(cfg.KeepAliveInterval) * time.Second)
	opts.SetAutoReconnect(cfg.AutoReconnect)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Duration(cfg.ReconnectInterval) * time.Second)
	opts.SetCleanSession(false)

	if cfg.TLS || cert != nil {
		tlsCfg, err := buildTLSConfig(cert)
		if err != nil {
			log.Printf("[WARN] Failed to build TLS config: %v", err)
		} else {
			opts.SetTLSConfig(tlsCfg)
		}
	}

	return opts
}
