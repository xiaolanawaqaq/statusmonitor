package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func decodeProbeTestConfig(t *testing.T, format, source string) map[string]any {
	t.Helper()
	var cfg map[string]any
	var err error
	if format == "yaml" {
		err = yaml.Unmarshal([]byte(source), &cfg)
	} else {
		err = json.Unmarshal([]byte(source), &cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProbeHTTPExpectedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	tests := []struct {
		name   string
		format string
		source string
		value  any
		wantOK bool
	}{
		{name: "YAML integer array", format: "yaml", source: "expect_status: [200, 204]", wantOK: true},
		{name: "JSON number array", format: "json", source: `{"expect_status":[200,204]}`, wantOK: true},
		{name: "YAML scalar integer", format: "yaml", source: "expect_status: 204", wantOK: true},
		{name: "JSON scalar number", format: "json", source: `{"expect_status":204}`, wantOK: true},
		{name: "legacy string array", format: "yaml", source: `expect_status: ["200", "204"]`, wantOK: true},
		{name: "typed integer array", value: []int{200, 204}, wantOK: true},
		{name: "typed float array", value: []float64{200, 204}, wantOK: true},
		{name: "typed string array", value: []string{"204"}, wantOK: true},
		{name: "default only allows 200"},
		{name: "explicit mismatch", value: 201},
		{name: "fractional status is invalid", value: []any{204.5}},
		{name: "malformed string is invalid", value: []any{"204-secret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{"expect_status": tt.value}
			if tt.source != "" {
				cfg = decodeProbeTestConfig(t, tt.format, tt.source)
			}
			cfg["url"] = server.URL
			result := ProbeChannel(context.Background(), Channel{Type: "http", Config: cfg}, 2*time.Second)
			if result.OK != tt.wantOK || result.StatusCode != http.StatusNoContent {
				t.Fatalf("unexpected result: %+v", result)
			}
			if tt.wantOK && result.Error != "" || !tt.wantOK && result.Error != "unexpected status 204" {
				t.Fatalf("unexpected error: %q", result.Error)
			}
			if result.TTFTMs != nil || result.LatencyMs == nil || *result.LatencyMs < 0 {
				t.Fatalf("invalid HTTP timing: %+v", result)
			}
		})
	}
}

func TestProbeHTTPBodyValidation(t *testing.T) {
	const limit = 1 << 20
	const secret = "private-keyword-value"
	tests := []struct {
		name    string
		body    string
		keyword string
		status  int
		wantErr string
	}{
		{name: "keyword matches", body: "service is ready now", keyword: "ready"},
		{name: "keyword mismatch is private", body: "service is down", keyword: secret, wantErr: "expected keyword not found"},
		{name: "empty body", body: ""},
		{name: "keyword at exact size limit", body: strings.Repeat("x", limit-5) + "ready", keyword: "ready"},
		{name: "exact size without keyword", body: strings.Repeat("x", limit)},
		{name: "too large despite matching prefix", body: "ready" + strings.Repeat("x", limit-4), keyword: "ready", wantErr: "response body too large"},
		{name: "too large without keyword", body: strings.Repeat("x", limit+1), wantErr: "response body too large"},
		{name: "unexpected status is also bounded", status: 503, body: strings.Repeat("x", limit+1), wantErr: "response body too large"},
		{name: "unexpected status hides body", status: 503, body: secret, wantErr: "unexpected status 503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			cfg := map[string]any{"url": server.URL, "expect_keyword": tt.keyword}
			result := ProbeChannel(context.Background(), Channel{Type: "http", Config: cfg}, 2*time.Second)
			if result.OK != (tt.wantErr == "") || result.Error != tt.wantErr || result.StatusCode != status {
				t.Fatalf("unexpected result: %+v", result)
			}
			if result.LatencyMs == nil || result.TTFTMs != nil {
				t.Fatalf("invalid HTTP timing: %+v", result)
			}
		})
	}
}

func TestProbeHTTPLatencyIncludesCompleteBody(t *testing.T) {
	const delay = 80 * time.Millisecond
	for _, keyword := range []string{"", "ready", "tail-marker"} {
		t.Run("keyword="+keyword, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "ready ")
				w.(http.Flusher).Flush()
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
					return
				case <-timer.C:
				}
				io.WriteString(w, "tail-marker")
			}))
			defer server.Close()
			cfg := map[string]any{"url": server.URL, "expect_keyword": keyword}
			result := ProbeChannel(context.Background(), Channel{Type: "http", Config: cfg}, 2*time.Second)
			if !result.OK || result.Error != "" || result.TTFTMs != nil {
				t.Fatalf("unexpected result: %+v", result)
			}
			if result.LatencyMs == nil || *result.LatencyMs < float64(delay/time.Millisecond) {
				t.Fatalf("latency did not include the delayed body: %+v", result)
			}
		})
	}
}

func TestProbeResponseReadFailures(t *testing.T) {
	modes := []struct {
		name    string
		kind    string
		keyword string
		stream  bool
		partial string
	}{
		{name: "HTTP without keyword", kind: "http", partial: "ready"},
		{name: "HTTP with matched keyword", kind: "http", keyword: "ready", partial: "ready"},
		{name: "OpenAI nonstream", kind: "openai", partial: `{"choices":[{"message":{"content":"ready"}}]}`},
		{name: "OpenAI stream", kind: "openai", stream: true, partial: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
	}
	for _, mode := range modes {
		for _, failure := range []string{"truncated", "timeout"} {
			t.Run(mode.name+"/"+failure, func(t *testing.T) {
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if failure == "truncated" {
						w.Header().Set("Content-Length", fmt.Sprint(len(mode.partial)+100))
					}
					io.WriteString(w, mode.partial)
					w.(http.Flusher).Flush()
					if failure == "timeout" {
						select {
						case <-r.Context().Done():
						case <-release:
						}
					}
				}))
				defer server.Close()
				defer close(release)
				cfg := map[string]any{
					"url": server.URL, "expect_keyword": mode.keyword,
					"base_url": server.URL, "model": "test-model", "api_key": "local-test-key", "stream": mode.stream,
				}
				timeout := 2 * time.Second
				wantErr := "response body read failed"
				if mode.stream {
					wantErr = "stream read failed"
				}
				if failure == "timeout" {
					timeout = 120 * time.Millisecond
					wantErr = "timeout"
				}
				result := ProbeChannel(context.Background(), Channel{Type: mode.kind, Config: cfg}, timeout)
				if result.OK || result.Error != wantErr || result.StatusCode != http.StatusOK || result.TTFTMs != nil {
					t.Fatalf("partial response must fail: %+v", result)
				}
				if result.LatencyMs == nil {
					t.Fatalf("missing failure latency: %+v", result)
				}
				if failure == "timeout" && *result.LatencyMs < 100 {
					t.Fatalf("latency omitted the body timeout: %v", *result.LatencyMs)
				}
			})
		}
	}
}

func TestProbeOpenAIStreamFirstContent(t *testing.T) {
	const delay = 80 * time.Millisecond
	t.Setenv("STATUS_MONITOR_PROBE_TEST_KEY", "local-test-key")
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Error("environment API key was not resolved")
		}
		var payload struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Model != "test-model" || !payload.Stream || payload.MaxTokens != 12 {
			t.Errorf("unexpected payload: %+v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": heartbeat\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\",\"content\":null}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{}}\n\n")
		w.(http.Flusher).Flush()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}},{\"delta\":{\"content\":\"OK\"}}]}\r\n\r\n")
		w.(http.Flusher).Flush()
		// Keep the stream open: the probe must return on text, without waiting for DONE.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	cfg := map[string]any{
		"base_url": server.URL + "/v1/", "api_key": "env:STATUS_MONITOR_PROBE_TEST_KEY",
		"model": "test-model", "max_tokens": float64(12),
	}
	result := ProbeChannel(context.Background(), Channel{Type: "openai", Config: cfg}, 2*time.Second)
	if !result.OK || result.Error != "" || result.StatusCode != http.StatusOK {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.TTFTMs == nil || *result.TTFTMs < float64(delay/time.Millisecond) {
		t.Fatalf("TTFT must wait for body text: %+v", result)
	}
	if result.LatencyMs == nil || *result.LatencyMs < *result.TTFTMs {
		t.Fatalf("invalid stream latency: %+v", result)
	}
}

func TestProbeOpenAIStreamWithoutContent(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty response"},
		{name: "role then DONE", body: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: [DONE]\n\n"},
		{name: "reasoning then EOF", body: `data: {"choices":[{"delta":{"reasoning_content":"private reasoning","content":null}}]}`},
		{name: "tool call only", body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call-1\"}]}}]}\n\n"},
		{name: "text after DONE is ignored", body: "data: [DONE]\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"},
		{name: "upstream error is private", body: "data: {\"error\":{\"message\":\"private-api-key\"}}\n\ndata: [DONE]\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			cfg := map[string]any{"base_url": server.URL, "api_key": "local-test-key", "model": "test-model"}
			result := ProbeChannel(context.Background(), Channel{Type: "openai", Config: cfg}, 2*time.Second)
			if result.OK || result.Error != "stream ended without content" || result.TTFTMs != nil || result.StatusCode != http.StatusOK {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestProbeOpenAIStreamLineLimit(t *testing.T) {
	const limit = 1 << 20
	const contentPrefix = `data: {"choices":[{"delta":{"content":"`
	const contentSuffix = `"}}]}`
	const text = "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "reasoning line larger than default scanner buffer",
			body: `data: {"choices":[{"delta":{"reasoning_content":"` + strings.Repeat("r", 96*1024) + `"}}]}` + "\n\n" + text,
		},
		{
			name: "exact limit with CRLF",
			body: contentPrefix + strings.Repeat("x", limit-len(contentPrefix)-len(contentSuffix)) + contentSuffix + "\r\n\r\n",
		},
		{name: "final content without newline", body: strings.TrimRight(text, "\n")},
		{
			name: "oversized content cannot succeed", wantErr: "stream line too large",
			body: contentPrefix + strings.Repeat("x", limit) + contentSuffix + "\n\n" + text,
		},
		{name: "oversized unterminated comment", body: ":" + strings.Repeat("x", limit+1), wantErr: "stream line too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			cfg := map[string]any{"base_url": server.URL, "api_key": "local-test-key", "model": "test-model"}
			result := ProbeChannel(context.Background(), Channel{Type: "openai", Config: cfg}, 2*time.Second)
			if result.OK != (tt.wantErr == "") || result.Error != tt.wantErr {
				t.Fatalf("unexpected result: %+v", result)
			}
			if (result.TTFTMs != nil) != result.OK {
				t.Fatalf("TTFT must only exist for body text: %+v", result)
			}
		})
	}
}

func TestProbeOpenAINonstream(t *testing.T) {
	const completion = `{"choices":[{"message":{"content":"OK"}}]}`
	const limit = 1 << 20
	tests := []struct {
		name    string
		body    string
		status  int
		wantErr string
	}{
		{name: "success", body: completion},
		{name: "exact limit", body: completion + strings.Repeat(" ", limit-len(completion))},
		{name: "oversized response", body: completion + strings.Repeat(" ", limit-len(completion)+1), wantErr: "response body too large"},
		{name: "empty content", body: `{"choices":[{"message":{"content":""}}]}`, wantErr: "empty completion"},
		{name: "missing choices", body: `{}`, wantErr: "empty completion"},
		{name: "invalid JSON is private", body: "private-api-key", wantErr: "invalid completion response"},
		{name: "upstream failure is private", status: 401, body: "private-api-key https://example.invalid/?token=private-token", wantErr: "unexpected status 401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			cfg := map[string]any{"base_url": server.URL, "api_key": "local-test-key", "model": "test-model", "stream": false}
			result := ProbeChannel(context.Background(), Channel{Type: "openai", Config: cfg}, 2*time.Second)
			if result.OK != (tt.wantErr == "") || result.Error != tt.wantErr || result.StatusCode != status {
				t.Fatalf("unexpected result: %+v", result)
			}
			if result.TTFTMs != nil || result.LatencyMs == nil {
				t.Fatalf("nonstream responses cannot report true TTFT: %+v", result)
			}
		})
	}
}

func TestProbeOpenAIMissingKeyDoesNotSendRequest(t *testing.T) {
	const envName = "STATUS_MONITOR_PROBE_TEST_UNSET_KEY"
	t.Setenv(envName, "")
	if err := os.Unsetenv(envName); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	for _, key := range []any{nil, "", " \t", "env:" + envName} {
		cfg := map[string]any{"base_url": server.URL, "model": "test-model", "api_key": key}
		result := ProbeChannel(context.Background(), Channel{Type: "openai", Config: cfg}, time.Second)
		if result.OK || result.Error != "missing API key" || result.StatusCode != 0 || result.TTFTMs != nil {
			t.Fatalf("missing key must fail before HTTP: %+v", result)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("missing key sent %d requests", got)
	}
}

func TestProbeRequestErrorsDoNotLeakSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	const secret = "private-api-key-and-token"
	for _, kind := range []string{"http", "openai"} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/invalid=%v", kind, invalid), func(t *testing.T) {
				url := server.URL + "/v1?api_key=" + secret
				if invalid {
					url += "\n"
				}
				cfg := map[string]any{"url": url, "base_url": url, "api_key": secret, "model": "test-model"}
				result := ProbeChannel(context.Background(), Channel{Type: kind, Config: cfg}, time.Second)
				label := "HTTP"
				if kind == "openai" {
					label = "OpenAI"
				}
				wantErr := label + " request failed"
				if invalid {
					wantErr = "invalid " + label + " request"
				}
				if result.OK || result.Error != wantErr || strings.Contains(result.Error, secret) || strings.Contains(result.Error, server.URL) {
					t.Fatalf("request error must be generic: %+v", result)
				}
			})
		}
	}
}

func TestProbeMockNumericConfig(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		source  string
		latency float64
		wantOK  bool
	}{
		{name: "YAML integer success", format: "yaml", source: "latency_ms: [2, 2]\nfail_rate: 0", latency: 2, wantOK: true},
		{name: "YAML integer failure", format: "yaml", source: "latency_ms: [2, 2]\nfail_rate: 1", latency: 2},
		{name: "YAML mixed numbers", format: "yaml", source: "latency_ms: [2, 2.0]\nfail_rate: 0.0", latency: 2, wantOK: true},
		{name: "JSON number success", format: "json", source: `{"latency_ms":[1.5,1.5],"fail_rate":0}`, latency: 1.5, wantOK: true},
		{name: "JSON number failure", format: "json", source: `{"latency_ms":[1.5,1.5],"fail_rate":1}`, latency: 1.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := decodeProbeTestConfig(t, tt.format, tt.source)
			result := ProbeChannel(context.Background(), Channel{Type: "mock", Config: cfg}, time.Second)
			if result.OK != tt.wantOK || result.LatencyMs == nil || *result.LatencyMs != tt.latency {
				t.Fatalf("numeric configuration was not honored: %+v", result)
			}
			if tt.wantOK && result.Error != "" || !tt.wantOK && result.Error != "mock failure" {
				t.Fatalf("unexpected error: %q", result.Error)
			}
		})
	}
}

func TestProbeMockContext(t *testing.T) {
	t.Run("already canceled with zero latency", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cfg := map[string]any{"latency_ms": []any{0, 0}}
		for i := 0; i < 20; i++ {
			result := ProbeChannel(ctx, Channel{Type: "mock", Config: cfg}, time.Second)
			if result.OK || result.Error != "canceled" || result.TTFTMs != nil {
				t.Fatalf("canceled context must always win: %+v", result)
			}
		}
	})
	t.Run("parent cancellation interrupts delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		timer := time.AfterFunc(20*time.Millisecond, cancel)
		defer timer.Stop()
		cfg := map[string]any{"latency_ms": []any{1000, 1000}}
		result := ProbeChannel(ctx, Channel{Type: "mock", Config: cfg}, 2*time.Second)
		if result.OK || result.Error != "canceled" || result.TTFTMs != nil {
			t.Fatalf("mock ignored cancellation: %+v", result)
		}
	})
	t.Run("probe timeout interrupts delay", func(t *testing.T) {
		cfg := map[string]any{"latency_ms": []any{1000, 1000}}
		result := ProbeChannel(context.Background(), Channel{Type: "mock", Config: cfg}, 20*time.Millisecond)
		if result.OK || result.Error != "timeout" || result.TTFTMs != nil {
			t.Fatalf("mock ignored timeout: %+v", result)
		}
	})
}

func TestProbeTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				if strings.Contains(string(buf[:n]), "ping") {
					c.Write([]byte("pong\n"))
				}
			}(conn)
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	res := ProbeChannel(context.Background(), Channel{Type: "tcp", Config: map[string]any{
		"host": host, "port": float64(port),
	}}, 3*time.Second)
	if !res.OK || res.LatencyMs == nil {
		t.Fatalf("tcp probe failed: %+v", res)
	}
	if res.TTFTMs != nil {
		t.Fatalf("tcp probe must not report TTFT: %v", res.TTFTMs)
	}

	res = ProbeChannel(context.Background(), Channel{Type: "tcp", Config: map[string]any{
		"host": host, "port": float64(port), "send": "ping\n", "expect": "pong",
	}}, 3*time.Second)
	if !res.OK {
		t.Fatalf("tcp handshake probe failed: %+v", res)
	}

	res = ProbeChannel(context.Background(), Channel{Type: "tcp", Config: map[string]any{
		"host": host, "port": float64(port), "send": "hello\n", "expect": "pong",
	}}, 3*time.Second)
	if res.OK {
		t.Fatalf("tcp probe should fail on unexpected response: %+v", res)
	}

	// Closed port must fail rather than hang.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedHost, closedPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	p, _ := strconv.Atoi(closedPort)
	res = ProbeChannel(context.Background(), Channel{Type: "tcp", Config: map[string]any{
		"host": closedHost, "port": float64(p),
	}}, 2*time.Second)
	if res.OK {
		t.Fatalf("closed port should fail: %+v", res)
	}
}

func TestValidateTCPChannel(t *testing.T) {
	if err := validateChannel(Channel{ID: "g1", Name: "Game", Type: "tcp"}); err == nil {
		t.Fatal("tcp channel without host/port must be rejected")
	}
	if err := validateChannel(Channel{ID: "g1", Name: "Game", Type: "tcp",
		Config: map[string]any{"host": "example.com", "port": float64(25565)}}); err != nil {
		t.Fatalf("valid tcp channel rejected: %v", err)
	}
	if err := validateChannel(Channel{ID: "g1", Name: "Game", Type: "tcp",
		Config: map[string]any{"host": "example.com", "port": float64(0)}}); err == nil {
		t.Fatal("port 0 must be rejected")
	}
}

// ---- fake Minecraft server: handshake + status request, then canned JSON ----
func startFakeMinecraft(t *testing.T, motd string, online, max int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				if _, err := mcReadPacket(r, 1024); err != nil { // handshake
					return
				}
				if _, err := mcReadPacket(r, 1024); err != nil { // status request
					return
				}
				status := fmt.Sprintf(
					`{"version":{"name":"1.21.1"},"players":{"online":%d,"max":%d},"description":{"text":"%s"}}`,
					online, max, motd)
				payload := append([]byte{0x00}, append(mcVarInt(int32(len(status))), status...)...)
				_ = mcWritePacket(c, payload)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestProbeMinecraft(t *testing.T) {
	addr := startFakeMinecraft(t, "生存服 测试服", 5, 50)
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	res := ProbeChannel(context.Background(), Channel{Type: "minecraft",
		Config: map[string]any{"host": host, "port": float64(port), "expect": "测试服"}}, 3*time.Second)
	if !res.OK {
		t.Fatalf("minecraft probe failed: %+v", res)
	}
	if res.Players == nil || *res.Players != 5 || res.MaxPlayers == nil || *res.MaxPlayers != 50 {
		t.Fatalf("players = %v/%v", res.Players, res.MaxPlayers)
	}
	if res.LatencyMs == nil {
		t.Fatal("latency missing")
	}

	// MOTD 不匹配要失败
	res = ProbeChannel(context.Background(), Channel{Type: "minecraft",
		Config: map[string]any{"host": host, "port": float64(port), "expect": "不存在的"}}, 3*time.Second)
	if res.OK {
		t.Fatal("expect mismatch should fail")
	}
}

func TestMCVarIntRoundtrip(t *testing.T) {
	cases := []int32{0, 1, 127, 128, 255, 25565, -1, -2147483648}
	for _, v := range cases {
		enc := mcVarInt(v)
		got, err := mcReadVarInt(bufio.NewReader(bytes.NewReader(enc)))
		if err != nil || got != int64(v) {
			t.Fatalf("varint %d -> %v (%v)", v, got, err)
		}
	}
	if len(mcVarInt(-1)) != 5 {
		t.Fatal("negative varint must be 5 bytes")
	}
}

// ---- fake Steam server: challenge then A2S_INFO reply ----
func startFakeSteam(t *testing.T, online, max int) (string, int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	addr := pc.LocalAddr().String()
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	go func() {
		buf := make([]byte, 2048)
		for {
			n, remote, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			base := append([]byte("\xFF\xFF\xFF\xFF"), []byte("TSource Engine Query\x00")...)
			if !bytes.HasPrefix(req, base) {
				continue
			}
			if len(req) == len(base) {
				// 还没带 challenge → 先下发 challenge
				challenge := []byte{0x01, 0x02, 0x03, 0x04}
				reply := append([]byte("\xFF\xFF\xFF\xFF\x41"), challenge...)
				pc.WriteTo(reply, remote)
				continue
			}
			// 带 challenge 的请求 → 回 A2S_INFO
			var reply bytes.Buffer
			reply.WriteString("\xFF\xFF\xFF\xFFI")
			reply.WriteByte(0x11)                           // protocol
			reply.WriteString("Test Server\x00")            // name
			reply.WriteString("de_map\x00")                 // map
			reply.WriteString("csgo\x00")                   // folder
			reply.WriteString("CS2\x00")                    // game
			reply.Write([]byte{0xDA, 0x02})                 // appid 730 LE
			reply.Write([]byte{byte(online), byte(max), 0}) // players, max, bots
			pc.WriteTo(reply.Bytes(), remote)
		}
	}()
	return host, port
}

func TestProbeSteam(t *testing.T) {
	host, port := startFakeSteam(t, 12, 32)
	res := ProbeChannel(context.Background(), Channel{Type: "steam",
		Config: map[string]any{"host": host, "port": float64(port)}}, 3*time.Second)
	if !res.OK {
		t.Fatalf("steam probe failed: %+v", res)
	}
	if res.Players == nil || *res.Players != 12 || res.MaxPlayers == nil || *res.MaxPlayers != 32 {
		t.Fatalf("players = %v/%v", res.Players, res.MaxPlayers)
	}
}

func TestProbeUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	host, portText, _ := net.SplitHostPort(pc.LocalAddr().String())
	port, _ := strconv.Atoi(portText)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, remote, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if bytes.Contains(buf[:n], []byte("ping")) {
				pc.WriteTo([]byte("MCPE;测试服"), remote)
			}
		}
	}()

	res := ProbeChannel(context.Background(), Channel{Type: "udp",
		Config: map[string]any{"host": host, "port": float64(port), "send": "ping\\n", "expect": "MCPE"}}, 3*time.Second)
	if !res.OK {
		t.Fatalf("udp probe failed: %+v", res)
	}
	res = ProbeChannel(context.Background(), Channel{Type: "udp",
		Config: map[string]any{"host": host, "port": float64(port), "send": "ping", "expect": "不匹配"}}, 3*time.Second)
	if res.OK {
		t.Fatal("expect mismatch should fail")
	}
}

func TestProbeTCPBannerOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			conn.Close()
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portText)

	res := ProbeChannel(context.Background(), Channel{Type: "tcp",
		Config: map[string]any{"host": host, "port": float64(port), "expect": "SSH-2.0"}}, 3*time.Second)
	if !res.OK {
		t.Fatalf("tcp banner probe failed: %+v", res)
	}
}

func TestProbeDNS(t *testing.T) {
	res := ProbeChannel(context.Background(), Channel{Type: "dns",
		Config: map[string]any{"host": "localhost", "expect": "127.0.0.1"}}, 3*time.Second)
	if !res.OK {
		t.Fatalf("dns probe failed: %+v", res)
	}
}

func TestValidateNewChannelTypes(t *testing.T) {
	bad := []Channel{
		{ID: "a", Name: "A", Type: "minecraft"},
		{ID: "a", Name: "A", Type: "steam", Config: map[string]any{"host": "h"}},
		{ID: "a", Name: "A", Type: "udp", Config: map[string]any{"host": "h", "port": float64(1)}},
		{ID: "a", Name: "A", Type: "icmp"},
		{ID: "a", Name: "A", Type: "dns"},
	}
	for i, c := range bad {
		if err := validateChannel(c); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
	good := []Channel{
		{ID: "a", Name: "A", Type: "minecraft", Config: map[string]any{"host": "h", "port": float64(25565)}},
		{ID: "a", Name: "A", Type: "steam", Config: map[string]any{"host": "h", "port": float64(27015)}},
		{ID: "a", Name: "A", Type: "udp", Config: map[string]any{"host": "h", "port": float64(19132), "send": "x"}},
		{ID: "a", Name: "A", Type: "icmp", Config: map[string]any{"host": "1.1.1.1"}},
		{ID: "a", Name: "A", Type: "dns", Config: map[string]any{"host": "example.com"}},
	}
	for i, c := range good {
		if err := validateChannel(c); err != nil {
			t.Fatalf("case %d unexpected error: %v", i, err)
		}
	}
}
