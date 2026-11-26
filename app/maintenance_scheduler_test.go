package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type maintenanceRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *maintenanceRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.events...)
}
func maintenanceScheduler(t *testing.T, s *Store) (*Scheduler, *maintenanceRecorder) {
	t.Helper()
	record := &maintenanceRecorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p AlertPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		record.mu.Lock()
		record.events = append(record.events, p.Event)
		record.mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(ts.Close)
	cfg := Config{Probe: ProbeConfig{TimeoutSeconds: 2, Concurrency: 1, IntervalSeconds: 60}, Alerts: AlertsConfig{Enabled: true, WebhookURL: ts.URL, FailThreshold: 2, RecoverThreshold: 1, LatencyThresholdMs: 10}}
	return NewScheduler(s, cfg), record
}
func maintenanceMock(id string, fail bool, latency int) Channel {
	rate := 0
	if fail {
		rate = 1
	}
	return Channel{ID: id, Name: id, Group: "test", Type: "mock", Config: map[string]any{"fail_rate": rate, "latency_ms": []any{latency, latency}}}
}
func runMaintenanceProbe(t *testing.T, sch *Scheduler, ch Channel) {
	t.Helper()
	if _, err := sch.ProbeNow(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceSuppressesFailureAndLatencyWhileRecording(t *testing.T) {
	s, now := maintenanceTestStore(t)
	sch, record := maintenanceScheduler(t, s)
	window, err := s.CreateMaintenance(MaintenanceInput{Title: "Game restart", EndAt: *now + 60, ChannelIDs: []string{"game"}})
	if err != nil {
		t.Fatal(err)
	}
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 20))
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 1))
	if len(record.list()) != 0 {
		t.Fatal("maintenance emitted notifications", record.list())
	}
	incs, err := s.Incidents(20)
	if err != nil || len(incs) != 0 {
		t.Fatal("maintenance created automated outage incident", incs, err)
	}
	var count, marked int
	if err := s.db.QueryRow(`SELECT COUNT(*),COUNT(maintenance_id) FROM probes WHERE channel_id='game'`).Scan(&count, &marked); err != nil {
		t.Fatal(err)
	}
	if count != 4 || marked != 4 {
		t.Fatalf("records count=%d marked=%d", count, marked)
	}
	snap, err := s.Snapshot(27000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Channels[0].Availability == nil || *snap.Channels[0].Availability != 50 {
		t.Fatal("maintenance falsified availability")
	}
	// The other service is not in the window and must still alert normally.
	runMaintenanceProbe(t, sch, maintenanceMock("web", true, 0))
	runMaintenanceProbe(t, sch, maintenanceMock("web", true, 0))
	if !reflect.DeepEqual(record.list(), []string{"down"}) {
		t.Fatal("scope suppression leaked to unrelated channel", record.list())
	}
	*now = window.EndAt
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	if len(record.list()) != 1 {
		t.Fatal("maintenance failures counted immediately after end")
	}
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 1))
	if !reflect.DeepEqual(record.list(), []string{"down", "down", "recovered"}) {
		t.Fatal("normal alerts not resumed", record.list())
	}
}

func TestMaintenanceCancelRestoresLatencyNotifications(t *testing.T) {
	s, now := maintenanceTestStore(t)
	sch, record := maintenanceScheduler(t, s)
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Slow work", EndAt: *now + 60, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 20))
	*now += 10
	if err := s.CancelMaintenance(m.ID); err != nil {
		t.Fatal(err)
	}
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 20))
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 20))
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 1))
	if !reflect.DeepEqual(record.list(), []string{"latency", "latency-recovered"}) {
		t.Fatal(record.list())
	}
}

func TestMaintenanceRestartRestoresOnlyPostWindowStreak(t *testing.T) {
	s, now := maintenanceTestStore(t)
	sch, record := maintenanceScheduler(t, s)
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Restart", EndAt: *now + 60, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	sch = NewScheduler(s, sch.cfg)
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	if len(record.list()) != 0 {
		t.Fatal("restart during maintenance unmuted notifications")
	}
	*now = m.EndAt
	sch = NewScheduler(s, sch.cfg)
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	if len(record.list()) != 0 {
		t.Fatal("old maintenance errors restored into streak")
	}
	sch = NewScheduler(s, sch.cfg)
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	if !reflect.DeepEqual(record.list(), []string{"down"}) {
		t.Fatal("post-window streak not restored across restart", record.list())
	}
}

func TestMaintenanceDoesNotClosePreexistingIncidents(t *testing.T) {
	s, now := maintenanceTestStore(t)
	sch, record := maintenanceScheduler(t, s)
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	runMaintenanceProbe(t, sch, maintenanceMock("game", true, 0))
	manualID, err := s.OpenManualIncident("game", "test", "manual event")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Recovery work", EndAt: *now + 60, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 1))
	open, err := s.OpenIncidentFor("game")
	if err != nil || open == nil {
		t.Fatal("maintenance auto-closed existing event")
	}
	*now = m.EndAt
	runMaintenanceProbe(t, sch, maintenanceMock("game", false, 1))
	if !reflect.DeepEqual(record.list(), []string{"down", "recovered"}) {
		t.Fatal(record.list())
	}
	incs, err := s.Incidents(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range incs {
		if in.ID == manualID && in.ResolvedAt != nil {
			t.Fatal("manual incident incorrectly closed")
		}
	}
}

func TestMaintenanceCreatedWhileProbeInFlight(t *testing.T) {
	s := newTestStore(t)
	var clock atomic.Int64
	clock.Store(1800000000)
	s.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	sch, record := maintenanceScheduler(t, s)
	sch.cfg.Alerts.FailThreshold = 1
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(503) }))
	defer upstream.Close()
	ch := Channel{ID: "game", Name: "game", Type: "http", Config: map[string]any{"url": upstream.URL}}
	if err := s.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := sch.ProbeNow(context.Background(), ch); done <- err }()
	<-entered
	clock.Add(1)
	if _, err := s.CreateMaintenance(MaintenanceInput{Title: "Started during probe", EndAt: clock.Load() + 60, AllChannels: true}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(record.list()) != 0 {
		t.Fatal("in-flight probe notified despite new maintenance")
	}
	var marked int
	if err := s.db.QueryRow(`SELECT COUNT(maintenance_id) FROM probes`).Scan(&marked); err != nil || marked != 1 {
		t.Fatal("crossing probe was not marked", marked, err)
	}
}
