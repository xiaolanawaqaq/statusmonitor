package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceAPIAuthLifecycleAndPublicIsolation(t *testing.T) {
	setupPublicAPI(t, APIConfig{Enabled: true, RateLimitRPM: 1000})
	oldToken := adminTok
	adminTok = "maintenance-test"
	t.Cleanup(func() { adminTok = oldToken })
	now := int64(1800000000)
	store.now = func() time.Time { return time.Unix(now, 0) }
	h := newHandler()
	call := func(method, path, body string, admin bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if admin {
			r.Header.Set("X-Admin-Token", adminTok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/admin/maintenance", "/api/admin/maintenance/1/cancel"} {
		if w := call("POST", path, `{}`, false); w.Code != 401 {
			t.Fatalf("unauthorized %s returned %d", path, w.Code)
		}
	}
	if w := call("POST", "/v1/maintenance", `{}`, true); w.Code != 405 {
		t.Fatal("public API permits maintenance writes")
	}
	if w := call("GET", "/v1/admin/maintenance", "", true); w.Code != 404 {
		t.Fatal("public API exposes admin management")
	}
	body := fmt.Sprintf(`{"title":"Game update","body":"Upgrading","start_at":%d,"end_at":%d,"channel_ids":["c1"]}`, now+10, now+60)
	w := call("POST", "/api/admin/maintenance", body, true)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	var m MaintenanceWindow
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.State != "scheduled" {
		t.Fatal(m)
	}
	for _, path := range []string{"/api/maintenance", "/v1/maintenance", "/api/admin/maintenance"} {
		w := call("GET", path, "", true)
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		var list []MaintenanceWindow
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 {
			t.Fatal(path, list, err)
		}
	}
	now += 10
	w = call("GET", "/api/status", "", false)
	var status StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Channels[0].Maintenance == nil || status.Channels[0].Maintenance.State != "active" || status.Channels[0].CurrentStatus != "partial" {
		t.Fatal("status missing maintenance", w.Body.String())
	}
	w = call("GET", "/api/status?format=lcapi", "", false)
	var compat struct {
		Groups []struct {
			Status string `json:"status"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &compat); err != nil || compat.Groups[0].Status != "yellow" {
		t.Fatal("compat maintenance mapping failed", w.Body.String())
	}
	w = call("GET", fmt.Sprintf("/api/dynamics/m%d", m.ID), "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"source":"scheduled"`) {
		t.Fatal("missing maintenance dynamic", w.Body.String())
	}
	now++
	w = call("POST", fmt.Sprintf("/api/admin/maintenance/%d/cancel", m.ID), `{}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", fmt.Sprintf("/api/admin/maintenance/%d/cancel", m.ID), `{}`, true); w.Code != 404 {
		t.Fatal("repeated cancel should return 404")
	}
	w = call("GET", "/api/maintenance", "", false)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("cancelled maintenance still upcoming")
	}
	w = call("GET", "/api/admin/maintenance", "", true)
	if !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Fatal("cancel record not retained")
	}
}

func TestMaintenanceAPIValidation(t *testing.T) {
	setupPublicAPI(t, APIConfig{})
	oldToken := adminTok
	adminTok = "maintenance-test"
	t.Cleanup(func() { adminTok = oldToken })
	h := newHandler()
	for _, body := range []string{`{}`, `null`, `[]`, `{"title":true}`, `{"title":"t","end_at":1}`, strings.Repeat("x", 65537), `{"title":"t"} {}`} {
		r := httptest.NewRequest("POST", "/api/admin/maintenance", strings.NewReader(body))
		r.Header.Set("X-Admin-Token", adminTok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid input returned %d", w.Code)
		}
	}
	for _, id := range []string{"0", "-1", "abc"} {
		r := httptest.NewRequest("POST", "/api/admin/maintenance/"+id+"/cancel", nil)
		r.Header.Set("X-Admin-Token", adminTok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(id, w.Code)
		}
	}
}
