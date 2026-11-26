package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func signedDingTalkURL(raw, secret string, timestamp int64) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || validateURL(raw) != nil {
		return "", fmt.Errorf("invalid receiver URL")
	}
	if secret == "" {
		return raw, nil
	}
	stamp := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(stamp + "\n" + secret))
	q := u.Query()
	q.Set("timestamp", stamp)
	q.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func feishuSignature(secret string, timestamp int64) string {
	// Feishu signs an empty message using timestamp + newline + secret as the HMAC key.
	key := strconv.FormatInt(timestamp, 10) + "\n" + secret
	mac := hmac.New(sha256.New, []byte(key))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func limitAlertBytes(text string, maxBytes int) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes - len("…")
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

func limitAlertRunes(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes-1]) + "…"
}

func postPlatformJSON(ctx context.Context, client *http.Client, endpoint string, payload any, headers map[string]string) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cannot encode notification")
	}
	return postPlatform(ctx, client, endpoint, body, "application/json", headers)
}

func postPlatform(ctx context.Context, client *http.Client, endpoint string, body []byte, contentType string, headers map[string]string) ([]byte, error) {
	if validateURL(endpoint) != nil {
		return nil, fmt.Errorf("invalid receiver URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid notification request")
	}
	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, fmt.Errorf("response could not be read")
	}
	return data, nil
}

func checkPlatformCode(body []byte, key string, success int) error {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("invalid API response")
	}
	var code int
	value, ok := response[key]
	if !ok || string(value) == "null" {
		return fmt.Errorf("invalid API response")
	}
	if err := json.Unmarshal(value, &code); err != nil {
		var text string
		if json.Unmarshal(value, &text) != nil {
			return fmt.Errorf("invalid API response")
		}
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return fmt.Errorf("invalid API response")
		}
		code = parsed
	}
	if code != success {
		return fmt.Errorf("API rejected notification (code %d)", code)
	}
	return nil
}

func sendPlatformAlert(ctx context.Context, client *http.Client, a AlertsConfig, receiver, text string) error {
	switch receiver {
	case "dingtalk":
		endpoint, err := signedDingTalkURL(envString(a.DingTalkWebhookURL), envString(a.DingTalkSecret), time.Now().UnixMilli())
		if err != nil {
			return err
		}
		payload := map[string]any{
			"msgtype": "text", "text": map[string]string{"content": limitAlertBytes(text, 16000)},
			"at": map[string]any{"isAtAll": false},
		}
		body, err := postPlatformJSON(ctx, client, endpoint, payload, nil)
		if err != nil {
			return err
		}
		return checkPlatformCode(body, "errcode", 0)
	case "feishu":
		payload := map[string]any{
			"msg_type": "text", "content": map[string]string{"text": limitAlertBytes(strings.ReplaceAll(text, "<at", "< at"), 16000)},
		}
		if secret := envString(a.FeishuSecret); secret != "" {
			stamp := time.Now().Unix()
			payload["timestamp"] = strconv.FormatInt(stamp, 10)
			payload["sign"] = feishuSignature(secret, stamp)
		}
		body, err := postPlatformJSON(ctx, client, envString(a.FeishuWebhookURL), payload, nil)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			return fmt.Errorf("invalid API response")
		}
		if _, ok := fields["code"]; ok {
			if err := checkPlatformCode(body, "code", 0); err != nil {
				return err
			}
			if _, ok := fields["StatusCode"]; !ok {
				return nil
			}
		}
		return checkPlatformCode(body, "StatusCode", 0)
	case "wecom":
		payload := map[string]any{
			"msgtype": "text", "text": map[string]string{"content": limitAlertBytes(text, 2000)},
		}
		body, err := postPlatformJSON(ctx, client, envString(a.WeComWebhookURL), payload, nil)
		if err != nil {
			return err
		}
		return checkPlatformCode(body, "errcode", 0)
	case "discord":
		payload := map[string]any{
			"content":          limitAlertRunes(text, 1900),
			"allowed_mentions": map[string]any{"parse": []string{}},
		}
		body, err := postPlatformJSON(ctx, client, envString(a.DiscordWebhookURL), payload, nil)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		var message struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &message) != nil || message.ID == "" {
			return fmt.Errorf("invalid API response")
		}
		return nil
	case "slack":
		text = limitAlertRunes(text, 2900)
		payload := map[string]any{
			"text":         strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text),
			"blocks":       []any{map[string]any{"type": "section", "text": map[string]any{"type": "plain_text", "text": text, "emoji": false}}},
			"unfurl_links": false, "unfurl_media": false,
		}
		body, err := postPlatformJSON(ctx, client, envString(a.SlackWebhookURL), payload, nil)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(body)) != "ok" {
			return fmt.Errorf("API rejected notification")
		}
		return nil
	case "bark":
		server := strings.TrimRight(envString(a.BarkServerURL), "/")
		if server == "" {
			server = "https://api.day.app"
		}
		payload := map[string]any{
			"device_key": envString(a.BarkDeviceKey), "title": "服务监控通知",
			"body": limitAlertBytes(text, 4000), "group": "status-monitor",
		}
		body, err := postPlatformJSON(ctx, client, server+"/push", payload, nil)
		if err != nil {
			return err
		}
		return checkPlatformCode(body, "code", 200)
	case "ntfy":
		headers := map[string]string{"Title": "Status Monitor"}
		if token := envString(a.NtfyToken); token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		body, err := postPlatform(ctx, client, envString(a.NtfyURL), []byte(limitAlertBytes(text, 4000)), "text/plain; charset=utf-8", headers)
		if err != nil {
			return err
		}
		var message struct {
			ID    string `json:"id"`
			Event string `json:"event"`
		}
		if json.Unmarshal(body, &message) != nil || message.ID == "" || message.Event != "message" {
			return fmt.Errorf("invalid API response")
		}
		return nil
	default:
		return fmt.Errorf("unknown alert receiver")
	}
}
