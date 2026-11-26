package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAlertConfigReceivers(t *testing.T) {
	cases := []struct {
		name    string
		alerts  AlertsConfig
		wantErr bool
	}{
		{"disabled", AlertsConfig{}, false},
		{"webhook only", AlertsConfig{Enabled: true, WebhookURL: "http://localhost/hook"}, false},
		{"telegram only", AlertsConfig{Enabled: true, TelegramToken: "fake-token", TelegramChatID: "-10042"}, false},
		{"both", AlertsConfig{Enabled: true, WebhookURL: "https://local.test/hook", TelegramToken: "fake-token", TelegramChatID: "42"}, false},
		{"missing receiver", AlertsConfig{Enabled: true}, true},
		{"token without chat", AlertsConfig{Enabled: true, TelegramToken: "fake-token"}, true},
		{"chat without token", AlertsConfig{Enabled: true, TelegramChatID: "42"}, true},
		{"bad webhook with valid telegram", AlertsConfig{Enabled: true, WebhookURL: "file:///tmp/hook", TelegramToken: "fake-token", TelegramChatID: "42"}, true},
		{"negative latency", AlertsConfig{LatencyThresholdMs: -1}, true},
		{"oversize latency", AlertsConfig{LatencyThresholdMs: 600001}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Alerts: tc.alerts}
			err := c.Normalize()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Normalize() = %v", err)
			}
		})
	}
}

func TestTelegramEnvironmentConfig(t *testing.T) {
	t.Setenv("TEST_TG_TOKEN", "fake-token")
	t.Setenv("TEST_TG_CHAT", "-10042")
	var c Config
	if err := yaml.Unmarshal([]byte("alerts:\n  enabled: true\n  telegram_token: env:TEST_TG_TOKEN\n  telegram_chat_id: env:TEST_TG_CHAT\n"), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Alerts.TelegramToken != "env:TEST_TG_TOKEN" {
		t.Fatal("environment reference must remain in config")
	}
	t.Setenv("TEST_TG_TOKEN", "")
	t.Setenv("TEST_TG_CHAT", "")
	if err := c.Normalize(); err == nil {
		t.Fatal("enabled but unresolved Telegram credentials must fail validation")
	}
	c.Alerts.Enabled = false
	if err := c.Normalize(); err != nil {
		t.Fatal("disabled template should accept empty environment references:", err)
	}
}

func TestChannelSortValidation(t *testing.T) {
	for _, sort := range []int{-1, 2147483648} {
		if err := validateChannel(Channel{ID: "mc", Name: "Game", Type: "mock", Sort: sort}); err == nil {
			t.Fatalf("sort %d should be rejected", sort)
		}
	}
	for _, sort := range []int{0, 1, 1000, 2147483647} {
		if err := validateChannel(Channel{ID: "mc", Name: "Game", Type: "mock", Sort: sort}); err != nil {
			t.Fatalf("sort %d: %v", sort, err)
		}
	}
}
