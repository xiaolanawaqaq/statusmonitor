package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type Config struct {
	Site     SiteConfig    `yaml:"site"`
	Server   ServerConfig  `yaml:"server"`
	Probe    ProbeConfig   `yaml:"probe"`
	Storage  StorageConfig `yaml:"storage"`
	Alerts   AlertsConfig  `yaml:"alerts"`
	Admin    AdminConfig   `yaml:"admin"`
	API      APIConfig     `yaml:"api"`
	Channels []Channel     `yaml:"channels"`
}

type APIConfig struct {
	Enabled      bool     `yaml:"enabled"`
	CORSOrigins  []string `yaml:"cors_origins"`
	RateLimitRPM int      `yaml:"rate_limit_rpm"`
	CacheSeconds int      `yaml:"cache_seconds"`
}

type ServerConfig struct {
	Addr             string `yaml:"addr"`
	TLSCert          string `yaml:"tls_cert"`
	TLSKey           string `yaml:"tls_key"`
	HTTPRedirectAddr string `yaml:"http_redirect_addr"`
}

type SiteConfig struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

type ProbeConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	TimeoutSeconds  int `yaml:"timeout_seconds"`
	Concurrency     int `yaml:"concurrency"`
}

type StorageConfig struct {
	Path          string `yaml:"path"`
	RetentionDays int    `yaml:"retention_days"`
}

type AlertsConfig struct {
	Enabled            bool   `yaml:"enabled"`
	WebhookURL         string `yaml:"webhook_url"`
	TelegramToken      string `yaml:"telegram_token"`
	TelegramChatID     string `yaml:"telegram_chat_id"`
	DingTalkWebhookURL string `yaml:"dingtalk_webhook_url"`
	DingTalkSecret     string `yaml:"dingtalk_secret"`
	FeishuWebhookURL   string `yaml:"feishu_webhook_url"`
	FeishuSecret       string `yaml:"feishu_secret"`
	WeComWebhookURL    string `yaml:"wecom_webhook_url"`
	DiscordWebhookURL  string `yaml:"discord_webhook_url"`
	SlackWebhookURL    string `yaml:"slack_webhook_url"`
	BarkServerURL      string `yaml:"bark_server_url"`
	BarkDeviceKey      string `yaml:"bark_device_key"`
	NtfyURL            string `yaml:"ntfy_url"`
	NtfyToken          string `yaml:"ntfy_token"`
	MessagePrefix      string `yaml:"message_prefix"`
	FailThreshold      int    `yaml:"fail_threshold"`
	RecoverThreshold   int    `yaml:"recover_threshold"`
	LatencyThresholdMs int    `yaml:"latency_threshold_ms"`
}

type AdminConfig struct {
	Token string `yaml:"token"`
}

type Channel struct {
	ID      string         `yaml:"id" json:"id"`
	Name    string         `yaml:"name" json:"name"`
	Group   string         `yaml:"group" json:"group"`
	Type    string         `yaml:"type" json:"type"`
	Sort    int            `yaml:"sort,omitempty" json:"sort"`
	Config  map[string]any `yaml:"config" json:"config"`
	Enabled *bool          `yaml:"enabled" json:"enabled"`
}

func (c Channel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func (c Channel) ConfigJSON() string {
	b, _ := json.Marshal(c.Config)
	return string(b)
}

func groupOr(g string) string {
	if g == "" {
		return "默认"
	}
	return g
}

func envString(value string) string {
	if strings.HasPrefix(value, "env:") {
		return os.Getenv(strings.TrimPrefix(value, "env:"))
	}
	return value
}

func (c *Config) Normalize() error {
	if c.Site.Title == "" {
		c.Site.Title = "Service Status"
	}
	if c.Probe.IntervalSeconds == 0 {
		c.Probe.IntervalSeconds = 60
	}
	if c.Probe.TimeoutSeconds == 0 {
		c.Probe.TimeoutSeconds = 30
	}
	if c.Probe.Concurrency == 0 {
		c.Probe.Concurrency = 8
	}
	if c.Server.Addr == "" {
		c.Server.Addr = "127.0.0.1:18080"
	}
	if c.Storage.Path == "" {
		c.Storage.Path = "data/status.db"
	}
	if c.Storage.RetentionDays == 0 {
		c.Storage.RetentionDays = 30
	}
	if c.Alerts.FailThreshold == 0 {
		c.Alerts.FailThreshold = 3
	}
	if c.Alerts.LatencyThresholdMs < 0 || c.Alerts.LatencyThresholdMs > 600000 {
		return fmt.Errorf("alerts.latency_threshold_ms must be 0 (off) or 1..600000")
	}
	if c.Alerts.RecoverThreshold == 0 {
		c.Alerts.RecoverThreshold = 1
	}
	if c.Probe.IntervalSeconds < 10 || c.Probe.IntervalSeconds > 86400 {
		return fmt.Errorf("probe.interval_seconds must be 10..86400")
	}
	if c.Probe.TimeoutSeconds < 1 || c.Probe.TimeoutSeconds > 120 {
		return fmt.Errorf("probe.timeout_seconds must be 1..120")
	}
	if c.Probe.Concurrency < 1 || c.Probe.Concurrency > 64 {
		return fmt.Errorf("probe.concurrency must be 1..64")
	}
	if c.Storage.RetentionDays < 1 || c.Storage.RetentionDays > 365 {
		return fmt.Errorf("storage.retention_days must be 1..365")
	}
	if c.Alerts.FailThreshold < 1 || c.Alerts.FailThreshold > 1000 || c.Alerts.RecoverThreshold < 1 || c.Alerts.RecoverThreshold > 1000 {
		return fmt.Errorf("alert thresholds must be 1..1000")
	}
	if err := validateAlertConfig(c.Alerts); err != nil {
		return err
	}
	if c.API.Enabled {
		if len(c.API.CORSOrigins) == 0 {
			c.API.CORSOrigins = []string{"*"}
		}
		for _, o := range c.API.CORSOrigins {
			if o == "*" {
				continue
			}
			u, err := url.Parse(o)
			if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" ||
				(u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("api.cors_origins entries must be * or http(s) origins: %q", o)
			}
		}
	}
	cert, key := envString(c.Server.TLSCert), envString(c.Server.TLSKey)
	if (cert == "") != (key == "") {
		return fmt.Errorf("server.tls_cert and server.tls_key must be set together")
	}
	if cert != "" {
		if _, err := os.Stat(cert); err != nil {
			return fmt.Errorf("server.tls_cert: %w", err)
		}
		if _, err := os.Stat(key); err != nil {
			return fmt.Errorf("server.tls_key: %w", err)
		}
		if c.Server.HTTPRedirectAddr != "" && c.Server.HTTPRedirectAddr == c.Server.Addr {
			return fmt.Errorf("server.http_redirect_addr must differ from server.addr")
		}
	} else if c.Server.HTTPRedirectAddr != "" {
		return fmt.Errorf("server.http_redirect_addr requires TLS to be configured")
	}
	seen := map[string]bool{}
	for _, ch := range c.Channels {
		if err := validateChannel(ch); err != nil {
			return fmt.Errorf("channel %s: %w", ch.ID, err)
		}
		if seen[ch.ID] {
			return fmt.Errorf("duplicate channel id %s", ch.ID)
		}
		seen[ch.ID] = true
	}
	return nil
}

var channelIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func validateURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("a valid http(s) URL without embedded credentials is required")
	}
	return nil
}

func validPort(cfg map[string]any) bool {
	port := cfgInt(cfg, "port", 0)
	return port >= 1 && port <= 65535
}

func validateChannel(ch Channel) error {
	if ch.Sort < 0 || ch.Sort > 2147483647 {
		return fmt.Errorf("sort must be an integer in 0..2147483647")
	}
	if !channelIDPattern.MatchString(ch.ID) || strings.TrimSpace(ch.Name) == "" || len(ch.Name) > 200 || len(ch.Group) > 200 {
		return fmt.Errorf("id must contain 1..64 letters, digits, underscores or hyphens; name is required (max 200 bytes)")
	}
	if v, ok := ch.Config["interval_seconds"]; ok {
		iv := cfgInt(ch.Config, "interval_seconds", -1)
		if iv < 10 || iv > 86400 {
			return fmt.Errorf("interval_seconds must be an integer in 10..86400")
		}
		if f, ok := v.(float64); ok && f != float64(iv) {
			return fmt.Errorf("interval_seconds must be an integer")
		}
	}
	switch ch.Type {
	case "openai", "http":
		key := "url"
		if ch.Type == "openai" {
			key = "base_url"
			if cfgString(ch.Config, "model", "") == "" {
				return fmt.Errorf("openai channel requires model")
			}
		}
		value := cfgString(ch.Config, key, "")
		if !strings.HasPrefix(value, "env:") {
			if err := validateURL(value); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	case "tcp":
		if cfgString(ch.Config, "host", "") == "" {
			return fmt.Errorf("tcp channel requires host")
		}
		if !validPort(ch.Config) {
			return fmt.Errorf("tcp channel requires port 1..65535")
		}
	case "minecraft", "steam", "udp":
		if cfgString(ch.Config, "host", "") == "" {
			return fmt.Errorf("%s channel requires host", ch.Type)
		}
		if !validPort(ch.Config) {
			return fmt.Errorf("%s channel requires port 1..65535", ch.Type)
		}
		if ch.Type == "udp" && cfgString(ch.Config, "send", "") == "" {
			return fmt.Errorf("udp channel requires send payload")
		}
	case "icmp", "dns":
		if cfgString(ch.Config, "host", "") == "" {
			return fmt.Errorf("%s channel requires host", ch.Type)
		}
	case "mock":
	default:
		return fmt.Errorf("type must be openai/http/tcp/minecraft/steam/udp/icmp/dns/mock")
	}
	return nil
}
