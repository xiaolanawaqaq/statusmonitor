package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNewAlertReceiverConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		alerts AlertsConfig
	}{
		{"dingtalk", AlertsConfig{DingTalkWebhookURL: "https://dingtalk.test/hook", DingTalkSecret: "SECtest"}},
		{"feishu", AlertsConfig{FeishuWebhookURL: "https://feishu.test/hook", FeishuSecret: "test"}},
		{"wecom", AlertsConfig{WeComWebhookURL: "https://wecom.test/hook"}},
		{"discord", AlertsConfig{DiscordWebhookURL: "https://discord.test/hook"}},
		{"slack", AlertsConfig{SlackWebhookURL: "https://slack.test/hook"}},
		{"bark default", AlertsConfig{BarkDeviceKey: "test-device"}},
		{"bark self hosted", AlertsConfig{BarkServerURL: "https://bark.test", BarkDeviceKey: "test-device"}},
		{"ntfy", AlertsConfig{NtfyURL: "https://ntfy.test/my-topic", NtfyToken: "tk_fake"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.alerts
			a.Enabled = true
			c := Config{Alerts: a}
			if err := c.Normalize(); err != nil {
				t.Fatal(err)
			}
			if !hasAlertReceiver(c.Alerts) {
				t.Fatal("receiver not detected")
			}
		})
	}
}

func TestAlertReceiverConfigRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		alerts AlertsConfig
	}{
		{"bad url", AlertsConfig{DingTalkWebhookURL: "ftp://user:secret@host/hook"}},
		{"embedded creds", AlertsConfig{WeComWebhookURL: "https://name:secret@host/hook"}},
		{"fragment", AlertsConfig{SlackWebhookURL: "https://host/hook#secret"}},
		{"missing dingtalk url", AlertsConfig{DingTalkSecret: "SECsecret"}},
		{"missing feishu url", AlertsConfig{FeishuSecret: "secret"}},
		{"missing bark key", AlertsConfig{BarkServerURL: "https://bark.test"}},
		{"missing ntfy topic", AlertsConfig{NtfyURL: "https://ntfy.test/"}},
		{"ntfy query", AlertsConfig{NtfyURL: "https://ntfy.test/topic?secret=abc"}},
		{"missing ntfy url", AlertsConfig{NtfyToken: "secret"}},
		{"ntfy header injection", AlertsConfig{NtfyURL: "https://ntfy.test/topic", NtfyToken: "secret\r\nX-Key: injected"}},
		{"prefix too long", AlertsConfig{MessagePrefix: strings.Repeat("字", 101)}},
		{"prefix newline", AlertsConfig{MessagePrefix: "abc\ndef"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.alerts
			a.Enabled = true
			a.WebhookURL = "https://fallback.test/hook"
			c := Config{Alerts: a}
			err := c.Normalize()
			if err == nil {
				t.Fatal("invalid receiver configuration accepted")
			}
			if strings.Contains(err.Error(), "SECsecret") || strings.Contains(err.Error(), "user:secret") {
				t.Fatalf("secret leaked: %v", err)
			}
		})
	}
}

func TestDingTalkEnvironmentAndSecretValidation(t *testing.T) {
	t.Setenv("TEST_DING_URL", "https://dingtalk.test/send?access_token=test")
	t.Setenv("TEST_DING_SECRET", "SECtest")
	var c Config
	if err := yaml.Unmarshal([]byte("alerts:\n  enabled: true\n  dingtalk_webhook_url: env:TEST_DING_URL\n  dingtalk_secret: env:TEST_DING_SECRET\n  message_prefix: 游戏监控\n"), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Alerts.DingTalkSecret != "env:TEST_DING_SECRET" {
		t.Fatal("secret reference should not be replaced in stored config")
	}
	t.Setenv("TEST_DING_SECRET", "")
	if err := c.Normalize(); err == nil {
		t.Fatal("configured but unresolved signing secret cannot silently disable signing")
	}
	t.Setenv("TEST_DING_URL", "")
	c.Alerts.Enabled = false
	if err := c.Normalize(); err != nil {
		t.Fatal("disabled template with unset references should remain usable:", err)
	}
}
