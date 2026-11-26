package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type AlertPayload struct {
	Event   string         `json:"event"`
	Text    string         `json:"text"`
	Channel map[string]any `json:"channel"`
	Detail  string         `json:"detail"`
	TS      int64          `json:"ts"`
}

type telegramResponse struct {
	OK bool `json:"ok"`
}

type AlertDelivery struct {
	Receiver string `json:"receiver"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

func SendAlert(ctx context.Context, alerts AlertsConfig, event string, ch Channel, detail string) error {
	return sendAlert(ctx, alertClient(), alerts, event, ch, detail)
}

func sendAlert(ctx context.Context, client *http.Client, alerts AlertsConfig, event string, ch Channel, detail string) error {
	_, err := deliverAlert(ctx, client, alerts, event, ch, detail, "")
	return err
}

func deliverAlert(ctx context.Context, client *http.Client, alerts AlertsConfig, event string, ch Channel, detail, target string) ([]AlertDelivery, error) {
	text := fmt.Sprintf("[%s] %s / %s: %s", event, ch.Group, ch.Name, detail)
	if prefix := strings.TrimSpace(alerts.MessagePrefix); prefix != "" {
		text = prefix + " " + text
	}
	selected := []AlertReceiver{}
	known := target == ""
	for _, receiver := range alertReceivers(alerts) {
		if receiver.ID == target {
			known = true
		}
		if receiver.Configured && (target == "" || target == receiver.ID) {
			selected = append(selected, receiver)
		}
	}
	if !known {
		return []AlertDelivery{}, fmt.Errorf("unknown alert receiver")
	}
	if len(selected) == 0 {
		return []AlertDelivery{}, fmt.Errorf("no alert receiver configured")
	}
	type response struct {
		index int
		err   error
	}
	finished := make(chan response, len(selected))
	for i, receiver := range selected {
		go func(i int, id string) {
			var err error
			switch id {
			case "webhook":
				err = sendWebhook(ctx, client, envString(alerts.WebhookURL), event, text, ch, detail)
			case "telegram":
				err = sendTelegram(ctx, client, envString(alerts.TelegramToken), envString(alerts.TelegramChatID), text)
			default:
				err = sendPlatformAlert(ctx, client, alerts, id, text)
			}
			finished <- response{i, err}
		}(i, receiver.ID)
	}
	results := make([]AlertDelivery, len(selected))
	for range selected {
		result := <-finished
		receiver := selected[result.index]
		delivery := AlertDelivery{Receiver: receiver.ID, Name: receiver.Name, OK: result.err == nil}
		if result.err != nil {
			delivery.Error = result.err.Error()
		}
		results[result.index] = delivery
	}
	var errs []string
	for _, result := range results {
		if !result.OK {
			errs = append(errs, result.Receiver+": "+result.Error)
		}
	}
	if len(errs) > 0 {
		return results, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return results, nil
}

func alertClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sendWebhook(ctx context.Context, client *http.Client, url, event, text string, ch Channel, detail string) error {
	if err := validateURL(url); err != nil {
		return fmt.Errorf("invalid webhook URL")
	}
	payload, err := json.Marshal(AlertPayload{
		Event: event, Text: text,
		Channel: map[string]any{"id": ch.ID, "name": ch.Name, "group": ch.Group},
		Detail:  detail, TS: time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("cannot encode alert")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("invalid webhook request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("returned HTTP %d", resp.StatusCode)
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 65536)); err != nil {
		return fmt.Errorf("response could not be read")
	}
	return nil
}

func sendTelegram(ctx context.Context, client *http.Client, token, chatID, text string) error {
	// Keep messages within Telegram's 4096-character limit, without splitting UTF-8.
	runes := []rune(text)
	if len(runes) > 4000 {
		text = string(runes[:3999]) + "…"
	}
	payload, err := json.Marshal(map[string]string{"chat_id": chatID, "text": text})
	if err != nil {
		return fmt.Errorf("cannot encode message")
	}
	url := "https://api.telegram.org/bot" + token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("invalid request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API rejected the message (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(body) > 65536 {
		return fmt.Errorf("response could not be read")
	}
	var tr telegramResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("invalid API response")
	}
	if !tr.OK {
		return fmt.Errorf("API rejected the message (HTTP %d)", resp.StatusCode)
	}
	return nil
}
