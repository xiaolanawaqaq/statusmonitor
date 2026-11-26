package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdminNotificationTargetsAndPartialResult(t *testing.T) {
	setupPublicAPI(t, APIConfig{})
	oldToken := adminTok
	adminTok = "notification-test-token"
	t.Cleanup(func() { adminTok = oldToken })
	var dingCount, wecomCount atomic.Int32
	ding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dingCount.Add(1)
		if r.URL.Query().Get("sign") == "" {
			t.Error("DingTalk signature missing")
		}
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"secret-error"}`))
	}))
	defer ding.Close()
	wecom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wecomCount.Add(1)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer wecom.Close()
	cfg.Alerts = AlertsConfig{Enabled: true, DingTalkWebhookURL: ding.URL + "?access_token=secret-access", DingTalkSecret: "SECsecret", WeComWebhookURL: wecom.URL + "?key=secret-key"}
	request := func(method, path, body, token string) (int, []byte) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Admin-Token", token)
		w := httptest.NewRecorder()
		newHandler().ServeHTTP(w, r)
		return w.Code, w.Body.Bytes()
	}
	code, body := request("GET", "/api/admin/alerts", "", adminTok)
	if code != 200 {
		t.Fatalf("metadata %d %s", code, body)
	}
	if strings.Contains(string(body), "secret") || strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("metadata exposed receiver credentials: %s", body)
	}
	var metadata struct {
		Enabled   bool            `json:"enabled"`
		Receivers []AlertReceiver `json:"receivers"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	if !metadata.Enabled || len(metadata.Receivers) != 9 {
		t.Fatalf("metadata=%+v", metadata)
	}
	if code, _ := request("GET", "/api/admin/alerts", "", ""); code != 401 {
		t.Fatal("receiver metadata must require admin authentication")
	}
	code, body = request("POST", "/api/admin/alerts/test", `{}`, adminTok)
	if code != 502 {
		t.Fatalf("partial failure=%d %s", code, body)
	}
	var result struct {
		OK      bool            `json:"ok"`
		Results []AlertDelivery `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || len(result.Results) != 2 || result.Results[0].OK || !result.Results[1].OK {
		t.Fatalf("result=%+v", result)
	}
	if strings.Contains(string(body), "secret") {
		t.Fatalf("delivery result exposed secret: %s", body)
	}
	code, body = request("POST", "/api/admin/alerts/test", `{"receiver":"wecom"}`, adminTok)
	if code != 200 {
		t.Fatalf("selected receiver failed: %d %s", code, body)
	}
	if dingCount.Load() != 1 || wecomCount.Load() != 2 {
		t.Fatal("selected test contacted extra receivers")
	}
	for _, bad := range []string{`{"receiver":"bogus"}`, `{"receiver":"bark"}`, `{"receiver":123}`, `{bad`, strings.Repeat("a", 1025)} {
		if code, _ := request("POST", "/api/admin/alerts/test", bad, adminTok); code != 400 {
			t.Fatalf("invalid target/body %q = %d", bad, code)
		}
	}
	if code, _ := request("GET", "/api/admin/alerts/test", "", adminTok); code != 405 {
		t.Fatal("test endpoint must require POST")
	}
	if dingCount.Load() != 1 || wecomCount.Load() != 2 {
		t.Fatal("invalid requests caused deliveries")
	}
	cfg.Alerts.Enabled = false
	if code, _ := request("POST", "/api/admin/alerts/test", `{}`, adminTok); code != 400 {
		t.Fatal("disabled alerts should not send")
	}
}
