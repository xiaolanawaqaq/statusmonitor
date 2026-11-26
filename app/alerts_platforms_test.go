package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func platformTestConfig() AlertsConfig {
	return AlertsConfig{
		DingTalkWebhookURL: "https://dingtalk.test/send?access_token=secret-token",
		DingTalkSecret:     "SECtest-secret", FeishuWebhookURL: "https://feishu.test/hook", FeishuSecret: "feishu-test-secret",
		WeComWebhookURL: "https://wecom.test/send?key=secret-key", DiscordWebhookURL: "https://discord.test/hook",
		SlackWebhookURL: "https://slack.test/hook", BarkServerURL: "https://bark.test", BarkDeviceKey: "device-secret",
		NtfyURL: "https://ntfy.test/status-topic", NtfyToken: "ntfy-secret",
	}
}

func TestRobotSignatureFixtures(t *testing.T) {
	u, err := signedDingTalkURL("https://dingtalk.test/send?access_token=abc&sign=old&timestamp=old", "SECtest-secret", 1700000000123)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("access_token") != "abc" || q.Get("timestamp") != "1700000000123" || q.Get("sign") != "y0pgYANLUSMjMfpcgR3VXhLNFtynW99C0SWp4U5wOwg=" || len(q["sign"]) != 1 {
		t.Fatalf("signature or query encoding incorrect: %v", q)
	}
	if got := feishuSignature("feishu-test-secret", 1700000000); got != "e3MJ8Q5llVxGJ8pVkGPBhexDAgjvYqpjPP1GkY9XceI=" {
		t.Fatalf("signature = %q", got)
	}
	raw := "https://dingtalk.test/send?access_token=abc"
	if got, err := signedDingTalkURL(raw, "", 1); err != nil || got != raw {
		t.Fatal("unsigned URL should remain unchanged")
	}
}

func TestNotificationPlatformRequests(t *testing.T) {
	cfg := platformTestConfig()
	text := "监控 [down] 游戏 / 世界服: 连接失败 @everyone <@123> <at user_id=\"all\">all</at>"
	for _, id := range []string{"dingtalk", "feishu", "wecom", "discord", "slack", "bark", "ntfy"} {
		t.Run(id, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != id+".test" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Host)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if id == "ntfy" {
					if string(raw) != text || r.URL.Path != "/status-topic" || r.Header.Get("Authorization") != "Bearer ntfy-secret" || !strings.Contains(r.Header.Get("Content-Type"), "text/plain") {
						t.Error("incorrect ntfy payload/auth")
					}
					return alertTestResponse(200, `{"id":"message1","event":"message"}`), nil
				}
				var payload map[string]any
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Error(err)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect JSON content type")
				}
				switch id {
				case "dingtalk":
					if payload["msgtype"] != "text" || payload["text"].(map[string]any)["content"] != text || payload["at"].(map[string]any)["isAtAll"] != false {
						t.Error("incorrect DingTalk text/mention payload")
					}
					q := r.URL.Query()
					if q.Get("timestamp") == "" || q.Get("sign") == "" || q.Get("access_token") != "secret-token" {
						t.Error("missing DingTalk auth/signature")
					}
					return alertTestResponse(200, `{"errcode":0,"errmsg":"ok"}`), nil
				case "feishu":
					if payload["msg_type"] != "text" || payload["sign"] == nil || payload["timestamp"] == nil {
						t.Error("missing Feishu message/signature")
					}
					content := payload["content"].(map[string]any)["text"].(string)
					if strings.Contains(content, "<at") {
						t.Error("Feishu text must not create mention tags")
					}
					return alertTestResponse(200, `{"code":0,"StatusCode":0}`), nil
				case "wecom":
					if payload["msgtype"] != "text" || payload["text"].(map[string]any)["content"] != text {
						t.Error("incorrect WeCom text payload")
					}
					if len(payload["text"].(map[string]any)) != 1 {
						t.Error("WeCom must not mention users")
					}
					return alertTestResponse(200, `{"errcode":0}`), nil
				case "discord":
					if payload["content"] != text || len(payload["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 {
						t.Error("incorrect Discord text/mention control")
					}
					return alertTestResponse(204, ""), nil
				case "slack":
					block := payload["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)
					if block["type"] != "plain_text" || block["text"] != text || strings.Contains(payload["text"].(string), "<@") {
						t.Error("Slack must use plain text and escape fallback mentions")
					}
					return alertTestResponse(200, "ok"), nil
				case "bark":
					if r.URL.Path != "/push" || payload["device_key"] != "device-secret" || payload["body"] != text || payload["group"] != "status-monitor" {
						t.Error("incorrect Bark push payload")
					}
					return alertTestResponse(200, `{"code":200,"message":"success"}`), nil
				}
				return nil, fmt.Errorf("unexpected receiver")
			})}
			if err := sendPlatformAlert(context.Background(), client, cfg, id, text); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d", requests)
			}
		})
	}
}

func TestPlatformBusinessErrorsAndResponses(t *testing.T) {
	for _, tc := range []struct {
		id     string
		body   string
		wantOK bool
	}{
		{"dingtalk", `{"errcode":"0"}`, true},
		{"dingtalk", `{"errcode":310000,"errmsg":"secret-token"}`, false},
		{"dingtalk", `{}`, false}, {"dingtalk", `{"errcode":null}`, false},
		{"feishu", `{"code":0}`, true}, {"feishu", `{"StatusCode":0}`, true},
		{"feishu", `{"code":19021,"msg":"secret-token"}`, false},
		{"feishu", `{"code":0,"StatusCode":1}`, false}, {"feishu", `{}`, false},
		{"wecom", `{"errcode":93000,"errmsg":"secret-key"}`, false},
		{"wecom", `{"errcode":"0"}`, true}, {"wecom", `{"errcode":false}`, false},
		{"discord", `{"id":"1234"}`, true}, {"discord", `{"message":"bad"}`, false},
		{"slack", `invalid_payload`, false}, {"slack", `ok\n`, false}, {"slack", "ok\n", true},
		{"bark", `{"code":400,"message":"device-secret"}`, false}, {"bark", `{"code":0}`, false},
		{"ntfy", `{"id":"1","event":"message"}`, true}, {"ntfy", `{"id":"1","event":"open"}`, false},
		{"ntfy", `{"error":"ntfy-secret"}`, false},
	} {
		t.Run(tc.id+"/"+tc.body, func(t *testing.T) {
			client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) { return alertTestResponse(200, tc.body), nil })}
			err := sendPlatformAlert(context.Background(), client, platformTestConfig(), tc.id, "test")
			if (err == nil) != tc.wantOK {
				t.Fatalf("err = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked credential: %v", err)
			}
		})
	}
}

func TestPlatformsRejectHTTPAndReadFailures(t *testing.T) {
	for _, receiver := range []string{"dingtalk", "feishu", "wecom", "discord", "slack", "bark", "ntfy"} {
		for _, mode := range []string{"http", "network", "malformed", "oversize"} {
			t.Run(receiver+"/"+mode, func(t *testing.T) {
				client := &http.Client{Transport: alertTestTransport(func(*http.Request) (*http.Response, error) {
					switch mode {
					case "http":
						return alertTestResponse(429, "secret upstream detail"), nil
					case "network":
						return nil, errors.New("https://example.test?token=secret")
					case "malformed":
						return alertTestResponse(200, "not-json"), nil
					default:
						return alertTestResponse(200, strings.Repeat("a", 65537)), nil
					}
				})}
				err := sendPlatformAlert(context.Background(), client, platformTestConfig(), receiver, "test")
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatalf("expected sanitized failure: %v", err)
				}
			})
		}
	}
}

func TestPlatformMessageLimitsPreserveUTF8(t *testing.T) {
	for _, id := range []string{"dingtalk", "feishu", "wecom", "discord", "slack", "bark", "ntfy"} {
		t.Run(id, func(t *testing.T) {
			client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				var payload map[string]any
				if id != "ntfy" && json.Unmarshal(raw, &payload) != nil {
					t.Error("invalid payload")
				}
				var text string
				maxBytes, maxRunes, response := 0, 0, ""
				switch id {
				case "dingtalk":
					text = payload["text"].(map[string]any)["content"].(string)
					maxBytes = 16000
					response = `{"errcode":0}`
				case "feishu":
					text = payload["content"].(map[string]any)["text"].(string)
					maxBytes = 16000
					response = `{"code":0}`
				case "wecom":
					text = payload["text"].(map[string]any)["content"].(string)
					maxBytes = 2000
					response = `{"errcode":0}`
				case "discord":
					text = payload["content"].(string)
					maxRunes = 1900
					response = `{"id":"1"}`
				case "slack":
					text = payload["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
					maxRunes = 2900
					response = "ok"
				case "bark":
					text = payload["body"].(string)
					maxBytes = 4000
					response = `{"code":200}`
				case "ntfy":
					text = string(raw)
					maxBytes = 4000
					response = `{"id":"1","event":"message"}`
				}
				if !utf8.ValidString(text) || !strings.HasSuffix(text, "…") || (maxBytes > 0 && len(text) > maxBytes) || (maxRunes > 0 && len([]rune(text)) > maxRunes) {
					t.Error("invalid message truncation")
				}
				return alertTestResponse(200, response), nil
			})}
			if err := sendPlatformAlert(context.Background(), client, platformTestConfig(), id, strings.Repeat("消息😀", 8000)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAllReceiversFanOutAndSelectiveTest(t *testing.T) {
	cfg := platformTestConfig()
	cfg.WebhookURL = "https://webhook.test/hook"
	cfg.TelegramToken = "fake"
	cfg.TelegramChatID = "42"
	cfg.MessagePrefix = "游戏监控"
	var mu sync.Mutex
	calls := map[string]int{}
	client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls[r.URL.Host]++
		mu.Unlock()
		responses := map[string]string{"webhook.test": "", "api.telegram.org": `{"ok":true}`, "dingtalk.test": `{"errcode":0}`, "feishu.test": `{"code":0}`, "wecom.test": `{"errcode":0}`, "discord.test": "", "slack.test": "ok", "bark.test": `{"code":200}`, "ntfy.test": `{"id":"1","event":"message"}`}
		return alertTestResponse(200, responses[r.URL.Host]), nil
	})}
	results, err := deliverAlert(context.Background(), client, cfg, "down", Channel{Name: "A"}, "failed", "")
	if err != nil || len(results) != 9 {
		t.Fatalf("results %+v err %v", results, err)
	}
	want := []string{"webhook", "telegram", "dingtalk", "feishu", "wecom", "discord", "slack", "bark", "ntfy"}
	got := []string{}
	for _, r := range results {
		got = append(got, r.Receiver)
		if !r.OK {
			t.Fatal(r)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	mu.Lock()
	if len(calls) != 9 {
		t.Errorf("calls = %v", calls)
	}
	calls = map[string]int{}
	mu.Unlock()
	results, err = deliverAlert(context.Background(), client, cfg, "test", Channel{}, "test", "dingtalk")
	if err != nil || len(results) != 1 || results[0].Receiver != "dingtalk" {
		t.Fatalf("results %+v err %v", results, err)
	}
	mu.Lock()
	if len(calls) != 1 || calls["dingtalk.test"] != 1 {
		t.Errorf("selective test contacted other receivers: %v", calls)
	}
	mu.Unlock()
}

func TestNotificationFailureDoesNotBlockOtherReceivers(t *testing.T) {
	cfg := platformTestConfig()
	cfg.FeishuWebhookURL = ""
	cfg.WeComWebhookURL = ""
	cfg.SlackWebhookURL = ""
	cfg.BarkDeviceKey = ""
	cfg.NtfyURL = ""
	done := make(chan struct{})
	client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "discord.test" {
			close(done)
			return alertTestResponse(204, ""), nil
		}
		select {
		case <-done:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return alertTestResponse(200, `{"errcode":310000}`), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results, err := deliverAlert(ctx, client, cfg, "test", Channel{}, "test", "")
	if err == nil || len(results) != 2 || results[0].OK || !results[1].OK {
		t.Fatalf("results %+v err %v", results, err)
	}
}
