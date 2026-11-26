package main

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

type AlertReceiver struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
}

func alertReceivers(a AlertsConfig) []AlertReceiver {
	return []AlertReceiver{
		{"webhook", "通用 Webhook", envString(a.WebhookURL) != ""},
		{"telegram", "Telegram", envString(a.TelegramToken) != "" && envString(a.TelegramChatID) != ""},
		{"dingtalk", "钉钉", envString(a.DingTalkWebhookURL) != ""},
		{"feishu", "飞书", envString(a.FeishuWebhookURL) != ""},
		{"wecom", "企业微信", envString(a.WeComWebhookURL) != ""},
		{"discord", "Discord", envString(a.DiscordWebhookURL) != ""},
		{"slack", "Slack", envString(a.SlackWebhookURL) != ""},
		{"bark", "Bark", envString(a.BarkDeviceKey) != ""},
		{"ntfy", "ntfy", envString(a.NtfyURL) != ""},
	}
}

func hasAlertReceiver(a AlertsConfig) bool {
	for _, r := range alertReceivers(a) {
		if r.Configured {
			return true
		}
	}
	return false
}

func validateAlertConfig(a AlertsConfig) error {
	if !utf8.ValidString(a.MessagePrefix) || len([]rune(a.MessagePrefix)) > 100 || strings.ContainsAny(a.MessagePrefix, "\r\n") {
		return fmt.Errorf("alerts.message_prefix must be one line of at most 100 characters")
	}
	pairs := []struct{ first, second, name string }{
		{a.TelegramToken, a.TelegramChatID, "telegram_token and telegram_chat_id"},
	}
	for _, p := range pairs {
		first, second := envString(p.first), envString(p.second)
		if (first == "") != (second == "") || (a.Enabled && (p.first != "" || p.second != "") && (first == "" || second == "")) {
			return fmt.Errorf("alerts.%s must both resolve to nonempty values", p.name)
		}
	}
	urls := []struct{ name, raw string }{
		{"webhook_url", a.WebhookURL}, {"dingtalk_webhook_url", a.DingTalkWebhookURL},
		{"feishu_webhook_url", a.FeishuWebhookURL}, {"wecom_webhook_url", a.WeComWebhookURL},
		{"discord_webhook_url", a.DiscordWebhookURL}, {"slack_webhook_url", a.SlackWebhookURL},
		{"bark_server_url", a.BarkServerURL}, {"ntfy_url", a.NtfyURL},
	}
	for _, field := range urls {
		value := envString(field.raw)
		if value == "" {
			if a.Enabled && field.raw != "" {
				return fmt.Errorf("alerts.%s environment variable is empty", field.name)
			}
			continue
		}
		u, err := url.Parse(value)
		if err != nil || validateURL(value) != nil || u.Fragment != "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("alerts.%s must be an http(s) URL without embedded credentials or fragment", field.name)
		}
		if field.name == "bark_server_url" && u.RawQuery != "" {
			return fmt.Errorf("alerts.bark_server_url must not include a query")
		}
		if field.name == "ntfy_url" && (strings.Trim(u.Path, "/") == "" || u.RawQuery != "") {
			return fmt.Errorf("alerts.ntfy_url must include the topic path and no query")
		}
	}
	dependencies := []struct{ name, raw, parent string }{
		{"dingtalk_secret", a.DingTalkSecret, a.DingTalkWebhookURL},
		{"feishu_secret", a.FeishuSecret, a.FeishuWebhookURL},
		{"ntfy_token", a.NtfyToken, a.NtfyURL},
		{"bark_server_url", a.BarkServerURL, a.BarkDeviceKey},
	}
	for _, field := range dependencies {
		if field.raw == "" {
			continue
		}
		value := envString(field.raw)
		if a.Enabled && (value == "" || envString(field.parent) == "") {
			return fmt.Errorf("alerts.%s requires a configured receiver and a nonempty resolved value", field.name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("alerts.%s cannot contain line breaks", field.name)
		}
	}
	if a.Enabled && a.BarkDeviceKey != "" && envString(a.BarkDeviceKey) == "" {
		return fmt.Errorf("alerts.bark_device_key environment variable is empty")
	}
	if a.Enabled && !hasAlertReceiver(a) {
		return fmt.Errorf("enabled alerts require at least one configured receiver")
	}
	return nil
}
