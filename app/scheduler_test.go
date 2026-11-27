package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func TestLatencyAlertTransitions(t *testing.T) {
	s := NewScheduler(nil, Config{Alerts: AlertsConfig{LatencyThresholdMs: 100}})
	ch := Channel{ID: "game"}
	steps := []struct {
		ok      bool
		latency *float64
		event   string
	}{
		{true, f64(80), ""},
		{true, f64(100), "latency"},
		{true, f64(500), ""},
		{false, f64(20), ""},
		{true, nil, ""},
		{true, f64(math.NaN()), ""},
		{true, f64(99), "latency-recovered"},
		{true, f64(20), ""},
		{true, f64(120), "latency"},
	}
	for i, step := range steps {
		event, detail := s.checkLatency(ch, ProbeResult{OK: step.ok, LatencyMs: step.latency})
		if event != step.event {
			t.Fatalf("step %d: event %q want %q", i, event, step.event)
		}
		if event != "" && detail == "" {
			t.Fatalf("step %d missing detail", i)
		}
	}
	if event, _ := s.checkLatency(Channel{ID: "other"}, ProbeResult{OK: true, LatencyMs: f64(20)}); event != "" {
		t.Fatal("latency state leaked between channels")
	}
	s.Forget(ch.ID)
	if event, _ := s.checkLatency(ch, ProbeResult{OK: true, LatencyMs: f64(10)}); event != "" {
		t.Fatal("forgotten channel must not emit a stale recovery")
	}
}

func TestLatencyAlertsDisabled(t *testing.T) {
	s := NewScheduler(nil, Config{})
	if event, _ := s.checkLatency(Channel{ID: "mc"}, ProbeResult{OK: true, LatencyMs: f64(10000)}); event != "" {
		t.Fatal("zero threshold must disable latency events")
	}
}

func TestLatencyAlertSurvivesRestart(t *testing.T) {
	store := newTestStore(t)
	var mu sync.Mutex
	events := []string{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload AlertPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		events = append(events, payload.Event)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	cfg := Config{
		Probe:  ProbeConfig{IntervalSeconds: 60, TimeoutSeconds: 2, Concurrency: 1},
		Alerts: AlertsConfig{Enabled: true, WebhookURL: receiver.URL, LatencyThresholdMs: 10, FailThreshold: 3, RecoverThreshold: 1},
	}
	ch := Channel{ID: "restart-test", Name: "Restart mock", Type: "mock", Config: map[string]any{"latency_ms": []any{40, 40}}}
	if err := store.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	if _, err := NewScheduler(store, cfg).ProbeNow(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	// A fresh scheduler stands in for a process restart: it must not repeat the alert
	// that the previous instance already delivered.
	if _, err := NewScheduler(store, cfg).ProbeNow(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string{}, events...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"latency"}) {
		t.Fatalf("events = %v, want one latency alert across restarts", got)
	}
}

func TestLatencyAlertsThroughProbePipeline(t *testing.T) {
	store := newTestStore(t)
	var mu sync.Mutex
	events := []string{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload AlertPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		events = append(events, payload.Event)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	cfg := Config{
		Probe:  ProbeConfig{IntervalSeconds: 60, TimeoutSeconds: 2, Concurrency: 1},
		Alerts: AlertsConfig{Enabled: true, WebhookURL: receiver.URL, LatencyThresholdMs: 10, FailThreshold: 3, RecoverThreshold: 1},
	}
	s := NewScheduler(store, cfg)
	ch := Channel{ID: "latency-test", Name: "Local mock", Type: "mock", Config: map[string]any{}}
	if err := store.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	for _, latency := range []int{20, 20, 2, 2} {
		ch.Config["latency_ms"] = []any{latency, latency}
		result, err := s.ProbeNow(context.Background(), ch)
		if err != nil || !result.OK {
			t.Fatalf("probe: %+v %v", result, err)
		}
	}
	mu.Lock()
	got := append([]string{}, events...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"latency", "latency-recovered"}) {
		t.Fatalf("events = %v", got)
	}
	incs, err := store.Incidents(10)
	if err != nil || len(incs) != 0 {
		t.Fatalf("slow successful probes must not open failure incidents: %+v %v", incs, err)
	}
	snap, err := store.Snapshot(27000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Channels[0].Probes != 4 || snap.Channels[0].Availability == nil || *snap.Channels[0].Availability != 100 {
		t.Fatalf("unexpected successful-probe statistics: %+v", snap.Channels[0])
	}
}

func TestConfigErrorDoesNotOpenIncidentOrAlert(t *testing.T) {
	store := newTestStore(t)
	var mu sync.Mutex
	var events []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p AlertPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		events = append(events, p.Event)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	cfg := Config{
		Probe:  ProbeConfig{IntervalSeconds: 60, TimeoutSeconds: 2, Concurrency: 1},
		Alerts: AlertsConfig{Enabled: true, WebhookURL: receiver.URL, LatencyThresholdMs: 5000, FailThreshold: 1, RecoverThreshold: 1},
	}
	s := NewScheduler(store, cfg)
	ch := Channel{ID: "no-key", Name: "No key", Type: "openai", Config: map[string]any{"base_url": "https://example.invalid/v1", "model": "x"}}
	if err := store.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		result, err := s.ProbeNow(context.Background(), ch)
		if err != nil {
			t.Fatal(err)
		}
		if !result.ConfigError {
			t.Fatalf("probe %d should be a config error: %+v", i, result)
		}
	}
	if incs, err := store.Incidents(10); err != nil || len(incs) != 0 {
		t.Fatalf("config errors must not open incidents: %+v %v", incs, err)
	}
	mu.Lock()
	got := append([]string{}, events...)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("config errors must not send alerts: %v", got)
	}
}
