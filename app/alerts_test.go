package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type alertTestTransport func(*http.Request) (*http.Response, error)

func (f alertTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func alertTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestSendAlertTelegram(t *testing.T) {
	t.Setenv("TEST_TG_TOKEN", "123:local-test")
	t.Setenv("TEST_TG_CHAT", "-10042")
	var got map[string]string
	client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Host != "api.telegram.org" || r.URL.Path != "/bot123:local-test/sendMessage" {
			t.Errorf("unexpected Telegram request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content type")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		return alertTestResponse(200, `{"ok":true,"result":{"message_id":1}}`), nil
	})}
	alerts := AlertsConfig{TelegramToken: "env:TEST_TG_TOKEN", TelegramChatID: "env:TEST_TG_CHAT"}
	err := sendAlert(context.Background(), client, alerts, "latency", Channel{ID: "mc", Name: "生存服", Group: "游戏"}, "响应变慢")
	if err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "-10042" || got["text"] != "[latency] 游戏 / 生存服: 响应变慢" {
		t.Fatalf("payload = %+v", got)
	}
}

func TestSendAlertTelegramRejectsInvalidResponses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"not authorized", 401, `{"ok":false,"description":"token-secret"}`},
		{"rate limited", 429, `{"ok":false}`},
		{"rejected", 200, `{"ok":false}`},
		{"invalid JSON", 200, `not JSON`},
		{"oversize", 200, strings.Repeat("x", 65537)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: alertTestTransport(func(*http.Request) (*http.Response, error) {
				return alertTestResponse(tt.status, tt.body), nil
			})}
			err := sendTelegram(context.Background(), client, "token-secret", "42", "test")
			if err == nil || strings.Contains(err.Error(), "token-secret") {
				t.Fatalf("expected sanitized error, got %v", err)
			}
		})
	}
}

func TestSendAlertTelegramTruncatesUTF8(t *testing.T) {
	client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		text := payload["text"]
		if !utf8.ValidString(text) || len([]rune(text)) != 4000 || !strings.HasSuffix(text, "…") {
			t.Error("Telegram text must be truncated without breaking UTF-8")
		}
		return alertTestResponse(200, `{"ok":true}`), nil
	})}
	if err := sendTelegram(context.Background(), client, "fake", "42", strings.Repeat("慢", 5000)); err != nil {
		t.Fatal(err)
	}
}

func TestSendAlertWebhook(t *testing.T) {
	var payload AlertPayload
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		close(done)
		w.WriteHeader(204)
	}))
	defer ts.Close()
	alerts := AlertsConfig{WebhookURL: ts.URL}
	ch := Channel{ID: "c1", Name: "C1", Group: "G"}
	if err := SendAlert(context.Background(), alerts, "down", ch, "连续 3 次失败"); err != nil {
		t.Fatal(err)
	}
	<-done
	if payload.Event != "down" || payload.Channel["id"] != "c1" || payload.Detail != "连续 3 次失败" || payload.Text == "" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestSendAlertNoReceiver(t *testing.T) {
	err := SendAlert(context.Background(), AlertsConfig{}, "down", Channel{ID: "c"}, "x")
	if err == nil || err.Error() != "no alert receiver configured" {
		t.Fatalf("err = %v", err)
	}
}

func TestSendAlertPartialFailureMerges(t *testing.T) {
	telegramDelivered := make(chan struct{})
	var once sync.Once
	client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.telegram.org" {
			once.Do(func() { close(telegramDelivered) })
			return alertTestResponse(200, `{"ok":true}`), nil
		}
		select {
		case <-telegramDelivered:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return alertTestResponse(500, "private upstream error"), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	alerts := AlertsConfig{WebhookURL: "http://local.test/hook", TelegramToken: "fake", TelegramChatID: "42"}
	err := sendAlert(ctx, client, alerts, "down", Channel{ID: "c"}, "x")
	if err == nil || !strings.Contains(err.Error(), "webhook: returned HTTP 500") || strings.Contains(err.Error(), "telegram:") {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-telegramDelivered:
	default:
		t.Fatal("a failing webhook must not block Telegram delivery")
	}
}

func TestSendAlertCancellation(t *testing.T) {
	client := &http.Client{Transport: alertTestTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, errors.New("private URL containing token")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sendAlert(ctx, client, AlertsConfig{TelegramToken: "secret", TelegramChatID: "42"}, "test", Channel{}, "")
	if err == nil || !strings.Contains(err.Error(), "telegram") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestSendAlertDoesNotFollowRedirect(t *testing.T) {
	var followed bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true }))
	defer dest.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	err := SendAlert(context.Background(), AlertsConfig{WebhookURL: source.URL}, "test", Channel{}, "")
	if err == nil || followed {
		t.Fatalf("redirect must fail without following: %v", err)
	}
}

func TestChannelSort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantIDs []string, wantSort []int) {
		t.Helper()
		chs, err := s.ListChannels(false)
		if err != nil {
			t.Fatal(err)
		}
		if len(chs) != len(wantIDs) {
			t.Fatalf("channels = %+v", chs)
		}
		for i := range chs {
			if chs[i].ID != wantIDs[i] || chs[i].Sort != wantSort[i] {
				t.Fatalf("channels = %+v; want %v with sort %v", chs, wantIDs, wantSort)
			}
		}
	}
	for _, ch := range []Channel{{ID: "b", Name: "B", Type: "mock"}, {ID: "a", Name: "A", Type: "mock", Sort: 1}} {
		if err := s.UpsertChannel(ch); err != nil {
			t.Fatal(err)
		}
	}
	check([]string{"a", "b"}, []int{1, 1000})
	if err := s.UpsertChannel(Channel{ID: "a", Name: "A2", Type: "mock"}); err != nil {
		t.Fatal(err)
	}
	check([]string{"a", "b"}, []int{1, 1000})
	if err := s.UpsertChannel(Channel{ID: "a", Name: "A", Type: "mock", Sort: 5000}); err != nil {
		t.Fatal(err)
	}
	check([]string{"b", "a"}, []int{1000, 5000})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check([]string{"b", "a"}, []int{1000, 5000})
}

func TestLastAlertDeliveryTracking(t *testing.T) {
	alertStatusMu.Lock()
	alertStatus = nil
	alertStatusMu.Unlock()
	if got := lastAlertDelivery(); got != nil {
		t.Fatalf("initial status = %+v, want nil", got)
	}
	ch := Channel{ID: "game", Name: "Game"}
	recordAlertDelivery("down", ch, errors.New("webhook: request failed"))
	got := lastAlertDelivery()
	if got == nil || got.OK || got.Event != "down" || got.Channel != "Game" || got.Error != "webhook: request failed" {
		t.Fatalf("failure status = %+v", got)
	}
	recordAlertDelivery("recovered", ch, nil)
	got = lastAlertDelivery()
	if got == nil || !got.OK || got.Event != "recovered" || got.Error != "" {
		t.Fatalf("success status = %+v", got)
	}
	// Callers must not be able to mutate the stored record through the returned pointer.
	got.Event = "tampered"
	if again := lastAlertDelivery(); again.Event != "recovered" {
		t.Fatalf("stored status was mutated: %+v", again)
	}
}
