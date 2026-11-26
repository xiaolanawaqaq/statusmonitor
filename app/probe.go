package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type ProbeResult struct {
	OK         bool     `json:"ok"`
	LatencyMs  *float64 `json:"latency_ms"`
	TTFTMs     *float64 `json:"ttft_ms"`
	StatusCode int      `json:"status_code"`
	Error      string   `json:"error"`
	Players    *int     `json:"players,omitempty"`
	MaxPlayers *int     `json:"max_players,omitempty"`
}

func f64(v float64) *float64 { return &v }

// resolveEnv 把 "env:VAR" 字符串替换成环境变量值，递归处理 map/list。
func resolveEnv(v any) any {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "env:") {
			return os.Getenv(strings.TrimPrefix(t, "env:"))
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = resolveEnv(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = resolveEnv(val)
		}
		return out
	default:
		return v
	}
}

func cfgString(cfg map[string]any, key, def string) string {
	if v, ok := cfg[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

func cfgInt(cfg map[string]any, key string, def int) int {
	if v, ok := cfg[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return def
}

func cfgBool(cfg map[string]any, key string, def bool) bool {
	if v, ok := cfg[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// cfgNumber accepts the numeric types produced by YAML and JSON decoding.
func cfgNumber(v any) (float64, bool) {
	var n float64
	switch v := v.(type) {
	case int:
		n = float64(v)
	case float64:
		n = v
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

func cfgFloatPair(cfg map[string]any, key string, lo, hi float64) (float64, float64) {
	var a, b any
	switch list := cfg[key].(type) {
	case []any:
		if len(list) == 2 {
			a, b = list[0], list[1]
		}
	case []int:
		if len(list) == 2 {
			a, b = list[0], list[1]
		}
	case []float64:
		if len(list) == 2 {
			a, b = list[0], list[1]
		}
	}
	x, xok := cfgNumber(a)
	y, yok := cfgNumber(b)
	if xok && yok {
		return x, y
	}
	return lo, hi
}

func cfgStringList(cfg map[string]any, key string) []string {
	if v, ok := cfg[key]; ok {
		if list, ok := v.([]any); ok {
			out := make([]string, 0, len(list))
			for _, item := range list {
				if s, ok := item.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

const (
	maxProbeBodyBytes       = 1 << 20
	maxProbeStreamLineBytes = 1 << 20
)

var errProbeBodyTooLarge = errors.New("response body too large")

func expectedHTTPStatuses(cfg map[string]any) map[int]bool {
	allowed := make(map[int]bool)
	add := func(value any) {
		if s, ok := value.(string); ok {
			code, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return
			}
			value = code
		}
		if n, ok := cfgNumber(value); ok && n >= 100 && n <= 599 && n == math.Trunc(n) {
			allowed[int(n)] = true
		}
	}
	switch values := cfg["expect_status"].(type) {
	case []any:
		for _, value := range values {
			add(value)
		}
	case []int:
		for _, value := range values {
			add(value)
		}
	case []float64:
		for _, value := range values {
			add(value)
		}
	case []string:
		for _, value := range values {
			add(value)
		}
	default:
		add(values)
	}
	if len(allowed) == 0 {
		allowed[http.StatusOK] = true
	}
	return allowed
}

func readProbeBody(r io.Reader) ([]byte, error) {
	// One extra byte distinguishes an exact-limit response from a truncated one.
	body, err := io.ReadAll(io.LimitReader(r, maxProbeBodyBytes+1))
	if len(body) > maxProbeBodyBytes {
		return nil, errProbeBodyTooLarge
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Never expose transport errors: they may contain URLs, credentials or body data.
func probeError(ctx context.Context, err error, fallback string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, errProbeBodyTooLarge) {
		return errProbeBodyTooLarge.Error()
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return "stream line too large"
	}
	return fallback
}

func ProbeChannel(ctx context.Context, ch Channel, timeout time.Duration) ProbeResult {
	cfg, _ := resolveEnv(ch.Config).(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return ProbeResult{Error: probeError(ctx, err, "probe failed")}
	}

	switch ch.Type {
	case "mock":
		return probeMock(ctx, cfg)
	case "http":
		return probeHTTP(ctx, cfg)
	case "openai":
		return probeOpenAI(ctx, cfg)
	case "tcp":
		return probeTCP(ctx, cfg)
	case "minecraft":
		return probeMinecraft(ctx, cfg)
	case "steam":
		return probeSteam(ctx, cfg)
	case "udp":
		return probeUDP(ctx, cfg)
	case "icmp":
		return probeICMP(ctx, cfg)
	case "dns":
		return probeDNS(ctx, cfg)
	default:
		return ProbeResult{Error: "unknown channel type"}
	}
}

func probeMock(ctx context.Context, cfg map[string]any) ProbeResult {
	if err := ctx.Err(); err != nil {
		return ProbeResult{Error: probeError(ctx, err, "mock failed")}
	}
	lo, hi := cfgFloatPair(cfg, "latency_ms", 200, 800)
	latency := lo + rand.Float64()*(hi-lo)
	timer := time.NewTimer(time.Duration(latency * float64(time.Millisecond)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ProbeResult{Error: probeError(ctx, ctx.Err(), "mock failed")}
	case <-timer.C:
	}
	// If the timer and cancellation were both ready, cancellation still wins.
	if err := ctx.Err(); err != nil {
		return ProbeResult{Error: probeError(ctx, err, "mock failed")}
	}
	failRate, _ := cfgNumber(cfg["fail_rate"])
	if rand.Float64() < failRate {
		return ProbeResult{LatencyMs: f64(latency), Error: "mock failure"}
	}
	return ProbeResult{OK: true, LatencyMs: f64(latency), TTFTMs: f64(latency * 0.6), StatusCode: 200}
}

func probeHTTP(ctx context.Context, cfg map[string]any) (result ProbeResult) {
	url := cfgString(cfg, "url", "")
	if url == "" {
		return ProbeResult{Error: "missing url"}
	}
	allowed := expectedHTTPStatuses(cfg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{Error: "invalid HTTP request"}
	}
	if headers, ok := cfg["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	start := time.Now()
	defer func() { result.LatencyMs = f64(float64(time.Since(start)) / float64(time.Millisecond)) }()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = probeError(ctx, err, "HTTP request failed")
		return result
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode

	body, err := readProbeBody(resp.Body)
	if err != nil {
		result.Error = probeError(ctx, err, "response body read failed")
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Error = probeError(ctx, err, "HTTP request failed")
		return result
	}
	if !allowed[resp.StatusCode] {
		result.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return result
	}
	if keyword := cfgString(cfg, "expect_keyword", ""); keyword != "" && !bytes.Contains(body, []byte(keyword)) {
		result.Error = "expected keyword not found"
		return result
	}

	result.OK = true
	return result
}

func probeOpenAI(ctx context.Context, cfg map[string]any) (result ProbeResult) {
	apiKey := cfgString(cfg, "api_key", "")
	if strings.TrimSpace(apiKey) == "" {
		return ProbeResult{Error: "missing API key"}
	}
	base := strings.TrimRight(cfgString(cfg, "base_url", ""), "/")
	if base == "" {
		return ProbeResult{Error: "missing base_url"}
	}
	model := cfgString(cfg, "model", "")
	if model == "" {
		return ProbeResult{Error: "missing model"}
	}
	url := base + "/chat/completions"
	stream := cfgBool(cfg, "stream", true)

	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": cfgString(cfg, "prompt", "Reply with the single word: OK")}},
		"max_tokens": cfgInt(cfg, "max_tokens", 16),
		"stream":     stream,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ProbeResult{Error: "invalid OpenAI request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if headers, ok := cfg["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	start := time.Now()
	defer func() { result.LatencyMs = f64(float64(time.Since(start)) / float64(time.Millisecond)) }()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = probeError(ctx, err, "OpenAI request failed")
		return result
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode

	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return result
	}

	if !stream {
		raw, err := readProbeBody(resp.Body)
		if err != nil {
			result.Error = probeError(ctx, err, "response body read failed")
			return result
		}
		if err := ctx.Err(); err != nil {
			result.Error = probeError(ctx, err, "OpenAI request failed")
			return result
		}
		var parsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			result.Error = "invalid completion response"
			return result
		}
		if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
			result.Error = "empty completion"
			return result
		}
		// A complete response does not reveal when its first token was generated.
		result.OK = true
		return result
	}

	// Bound each SSE line, including role/reasoning chunks we do not count as text.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxProbeStreamLineBytes+2)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			result.Error = probeError(ctx, err, "stream read failed")
			return result
		}
		if err := scanner.Err(); err != nil {
			result.Error = probeError(ctx, err, "stream read failed")
			return result
		}
		line := scanner.Text()
		if len(line) > maxProbeStreamLineBytes {
			result.Error = "stream line too large"
			return result
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				result.TTFTMs = f64(float64(time.Since(start)) / float64(time.Millisecond))
				result.OK = true
				return result
			}
		}
	}
	if err := scanner.Err(); err != nil {
		result.Error = probeError(ctx, err, "stream read failed")
	} else if err := ctx.Err(); err != nil {
		result.Error = probeError(ctx, err, "stream read failed")
	} else {
		result.Error = "stream ended without content"
	}
	return result
}

// probeTCP tests a raw TCP endpoint — game servers, databases, SSH, etc.
// Optional send/expect perform a tiny text handshake (e.g. a game status ping).
func probeTCP(ctx context.Context, cfg map[string]any) ProbeResult {
	host := cfgString(cfg, "host", "")
	port := cfgInt(cfg, "port", 0)
	if host == "" || port < 1 || port > 65535 {
		return ProbeResult{Error: "tcp channel requires host and port"}
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	start := time.Now()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{LatencyMs: f64(latency), Error: "timeout"}
		}
		return ProbeResult{LatencyMs: f64(latency), Error: "connection failed"}
	}
	defer conn.Close()

	expect := cfgString(cfg, "expect", "")
	send := unescape(cfgString(cfg, "send", ""))
	if send != "" {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte(send)); err != nil {
			return ProbeResult{LatencyMs: f64(latency), Error: "write failed"}
		}
	}
	if expect != "" {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		total := 0
		for total < len(buf) {
			n, err := conn.Read(buf[total:])
			total += n
			if err != nil || n == 0 {
				break
			}
			if bytes.Contains(buf[:total], []byte(expect)) {
				break
			}
		}
		if total == 0 {
			return ProbeResult{LatencyMs: f64(latency), Error: "no response"}
		}
		if !bytes.Contains(buf[:total], []byte(expect)) {
			return ProbeResult{LatencyMs: f64(latency), Error: "unexpected response"}
		}
	}
	return ProbeResult{OK: true, LatencyMs: f64(latency), StatusCode: 0}
}
