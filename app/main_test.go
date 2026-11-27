package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// setupPublicAPI 把全局 store/cfg 指到测试实例并返回 httptest 服务。
// 调用方负责恢复全局状态（本包测试串行执行，无需并行保护）。
func setupPublicAPI(t *testing.T, api APIConfig) *httptest.Server {
	t.Helper()
	oldCfg, oldStore := cfg, store
	dir := t.TempDir()
	testStore, err := OpenStore(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	store = testStore
	t.Cleanup(func() {
		testStore.Close()
		cfg, store = oldCfg, oldStore
	})

	enabled := true
	if _, err := store.SeedChannels([]Channel{{ID: "c1", Name: "C1", Type: "mock", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	lat := 42.0
	if err := store.AddProbe("c1", ProbeResult{OK: true, LatencyMs: &lat, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}

	cfg = Config{
		Site: SiteConfig{Title: "Test", Description: "desc"},
		API:  api,
	}

	ts := httptest.NewServer(newHandler())
	t.Cleanup(ts.Close)
	return ts
}

func TestPublicAPIBasics(t *testing.T) {
	ts := setupPublicAPI(t, APIConfig{
		Enabled:      true,
		CORSOrigins:  []string{"https://app.example.com"},
		RateLimitRPM: 1000,
		CacheSeconds: 60,
	})
	client := ts.Client()

	// 1) 状态接口 + CORS 白名单
	req, _ := http.NewRequest("GET", ts.URL+"/v1/status", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao != "https://app.example.com" {
		t.Fatalf("ACAO = %q", acao)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=60") {
		t.Fatalf("cache-control = %q", cc)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["channels"]; !ok {
		t.Fatalf("missing channels: %v", body)
	}

	// 2) 非白名单 Origin 不发 CORS 头
	req, _ = http.NewRequest("GET", ts.URL+"/v1/status", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("ACAO should be absent for unknown origin")
	}

	// 3) 预检请求
	req, _ = http.NewRequest("OPTIONS", ts.URL+"/v1/status", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp3, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 204 {
		t.Fatalf("preflight = %d", resp3.StatusCode)
	}

	// 4) 缓存命中：同一查询第二次应是 X-Cache: HIT 且内容一致
	get := func() (*http.Response, string) {
		r, err := client.Get(ts.URL + "/v1/status?window=27000")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		return r, string(buf)
	}
	r1, b1 := get()
	r2, b2 := get()
	if r1.Header.Get("X-Cache") != "MISS" || r2.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("cache headers: %q then %q", r1.Header.Get("X-Cache"), r2.Header.Get("X-Cache"))
	}
	if b1 != b2 {
		t.Fatal("cached body differs")
	}

	// 5) 管理接口不在公开面
	resp5, err := client.Get(ts.URL + "/v1/admin/channels")
	if err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != 404 {
		t.Fatalf("admin via /v1 = %d, want 404", resp5.StatusCode)
	}

	// 6) dynamics 与 health
	for _, path := range []string{"/v1/dynamics", "/v1/health", "/v1/history?days=7"} {
		r, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s = %d", path, r.StatusCode)
		}
	}

	// 7) 未知端点
	r, _ := client.Get(ts.URL + "/v1/nope")
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("unknown = %d", r.StatusCode)
	}
}

func TestPublicAPIRateLimit(t *testing.T) {
	ts := setupPublicAPI(t, APIConfig{
		Enabled:      true,
		CORSOrigins:  []string{"*"},
		RateLimitRPM: 5,
		CacheSeconds: 0, // 关闭缓存，保证每次都过限流器
	})
	client := ts.Client()

	codes := []int{}
	for i := 0; i < 8; i++ {
		r, err := client.Get(ts.URL + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, r.StatusCode)
		r.Body.Close()
	}
	passed, limited := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			passed++
		case 429:
			limited++
		}
	}
	if passed != 5 || limited != 3 {
		t.Fatalf("codes = %v, want 5x200 + 3x429", codes)
	}
}

func TestPublicAPIDisabled(t *testing.T) {
	ts := setupPublicAPI(t, APIConfig{Enabled: false})
	r, err := ts.Client().Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("disabled /v1/status = %d, want 404", r.StatusCode)
	}
	// 内部接口不受影响
	r2, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("/api/status = %d", r2.StatusCode)
	}
}

func TestAdminChannelsExposeConfigErrors(t *testing.T) {
	oldCfg, oldStore := cfg, store
	testStore, err := OpenStore(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	store = testStore
	t.Cleanup(func() {
		testStore.Close()
		cfg, store = oldCfg, oldStore
	})
	enabled := true
	if _, err := testStore.SeedChannels([]Channel{
		{ID: "configured", Name: "Configured", Type: "mock", Enabled: &enabled},
		{ID: "broken", Name: "Broken", Type: "openai", Enabled: &enabled},
	}); err != nil {
		t.Fatal(err)
	}
	lat := 10.0
	if err := testStore.AddProbe("configured", ProbeResult{OK: true, LatencyMs: &lat}); err != nil {
		t.Fatal(err)
	}
	if err := testStore.AddProbe("broken", ProbeResult{Error: "missing API key", ConfigError: true}); err != nil {
		t.Fatal(err)
	}
	cfg = Config{Admin: AdminConfig{Token: "test-token"}}
	oldTok := adminTok
	adminTok = "test-token"
	t.Cleanup(func() { adminTok = oldTok })

	ts := httptest.NewServer(newHandler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/api/admin/channels", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var rows []struct {
		ID            string `json:"id"`
		CurrentStatus string `json:"current_status"`
		ConfigError   bool   `json:"config_error"`
		LastError     string `json:"last_error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, r := range rows {
		byID[r.ID] = i
	}
	broken := rows[byID["broken"]]
	if broken.CurrentStatus != "config_error" || !broken.ConfigError || broken.LastError != "missing API key" {
		t.Fatalf("broken channel = %+v", broken)
	}
	good := rows[byID["configured"]]
	if good.CurrentStatus != "ok" || good.ConfigError {
		t.Fatalf("configured channel = %+v", good)
	}
}

func TestHealthReflectsProbeFreshness(t *testing.T) {
	ts := setupPublicAPI(t, APIConfig{})
	oldStart := startedAt
	startedAt = time.Now().Add(-time.Hour)
	t.Cleanup(func() { startedAt = oldStart })

	health := func() (int, map[string]any) {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + "/api/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}

	if code, body := health(); code != 200 || body["ok"] != true {
		t.Fatalf("fresh probe: %d %v", code, body)
	}

	// A monitor that stops writing probes must be reported as unhealthy.
	if _, err := store.db.Exec(`UPDATE probes SET ts = ?`, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	code, body := health()
	if code != 503 || body["ok"] != false {
		t.Fatalf("stale probe: %d %v", code, body)
	}
	if _, ok := body["last_probe_age_seconds"]; !ok {
		t.Fatalf("stale probe response lacks age: %v", body)
	}

	// With every channel disabled there is nothing to probe, so it is not unhealthy.
	if _, err := store.db.Exec(`UPDATE channels SET enabled = 0`); err != nil {
		t.Fatal(err)
	}
	if code, body := health(); code != 200 || body["ok"] != true {
		t.Fatalf("no enabled channels: %d %v", code, body)
	}
}

func TestHealthAllowsStartupGrace(t *testing.T) {
	ts := setupPublicAPI(t, APIConfig{})
	oldStart := startedAt
	startedAt = time.Now()
	t.Cleanup(func() { startedAt = oldStart })
	if _, err := store.db.Exec(`UPDATE probes SET ts = ?`, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("startup grace status = %d", resp.StatusCode)
	}
}

func TestPublicStateIsBounded(t *testing.T) {
	c := newPublicCache(10)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		c.put("GET /v1/status?x="+strconv.Itoa(i), cacheEntry{body: []byte("x"), status: 200, at: now}, now)
	}
	if len(c.m) > 256 {
		t.Fatalf("cache entries = %d, want <= 256", len(c.m))
	}

	l := newPublicLimiter(120)
	for i := 0; i < 6000; i++ {
		l.allow("10.0."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256), now)
	}
	if len(l.buckets) > 4096 {
		t.Fatalf("limiter buckets = %d, want <= 4096", len(l.buckets))
	}
}
