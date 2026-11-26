package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func maintenanceTestStore(t *testing.T) (*Store, *int64) {
	t.Helper()
	s := newTestStore(t)
	now := int64(1800000000)
	s.now = func() time.Time { return time.Unix(now, 0) }
	if _, err := s.SeedChannels([]Channel{{ID: "game", Name: "Game", Type: "mock"}, {ID: "web", Name: "Website", Type: "mock"}}); err != nil {
		t.Fatal(err)
	}
	return s, &now
}

func TestMaintenanceLifecycleAndScope(t *testing.T) {
	s, now := maintenanceTestStore(t)
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Scheduled restart", StartAt: *now + 60, EndAt: *now + 180, ChannelIDs: []string{"game"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "scheduled" || m.AllChannels || len(m.ChannelIDs) != 1 {
		t.Fatalf("window %+v", m)
	}
	if got, err := s.MaintenanceForProbe("game", *now, *now); err != nil || got != nil {
		t.Fatalf("future maintenance suppresses probes: %+v %v", got, err)
	}
	*now = m.StartAt
	got, err := s.MaintenanceForProbe("game", *now, *now)
	if err != nil || got == nil || got.State != "active" {
		t.Fatalf("start boundary: %+v %v", got, err)
	}
	if got, err := s.MaintenanceForProbe("web", *now, *now); err != nil || got != nil {
		t.Fatalf("unrelated channel: %+v %v", got, err)
	}
	list, err := s.ListMaintenance(true, 200)
	if err != nil || len(list) != 1 || list[0].State != "active" {
		t.Fatalf("list %+v %v", list, err)
	}
	*now = m.EndAt
	if got, err := s.MaintenanceForProbe("game", *now, *now); err != nil || got != nil {
		t.Fatalf("end boundary: %+v %v", got, err)
	}
	got, err = s.GetMaintenance(m.ID)
	if err != nil || got.State != "completed" {
		t.Fatalf("automatic completion: %+v %v", got, err)
	}
	list, err = s.ListMaintenance(true, 200)
	if err != nil || len(list) != 0 {
		t.Fatalf("finished window still public: %+v %v", list, err)
	}
	if !errors.Is(s.CancelMaintenance(m.ID), sql.ErrNoRows) {
		t.Fatal("completed window must not be cancellable")
	}
}

func TestMaintenanceCancellationAndOverlap(t *testing.T) {
	s, now := maintenanceTestStore(t)
	future, err := s.CreateMaintenance(MaintenanceInput{Title: "Future", StartAt: *now + 100, EndAt: *now + 200, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelMaintenance(future.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelMaintenance(future.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cancellation should not repeat")
	}
	*now += 110
	if got, err := s.MaintenanceForProbe("game", *now, *now); err != nil || got != nil {
		t.Fatal("cancelled future window became active")
	}
	first, err := s.CreateMaintenance(MaintenanceInput{Title: "First", EndAt: *now + 100, ChannelIDs: []string{"game"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateMaintenance(MaintenanceInput{Title: "Second", EndAt: *now + 200, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	*now += 20
	if err := s.CancelMaintenance(second.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.MaintenanceForProbe("game", *now, *now)
	if err != nil || got == nil || got.ID != first.ID {
		t.Fatal("ending overlapping window removed remaining coverage", got, err)
	}
	if got, err := s.MaintenanceForProbe("web", *now, *now); err != nil || got != nil {
		t.Fatal("ended all-channel window still active")
	}
	if got, err := s.MaintenanceForProbe("web", *now-2, *now); err != nil || got == nil || got.ID != second.ID {
		t.Fatal("probe crossing cancellation must be marked maintenance", got, err)
	}
	if end, err := s.LatestMaintenanceEnd("web", *now); err != nil || end != *now {
		t.Fatal("effective end missing", end, err)
	}
}

func TestMaintenanceAllChannelsIncludesNewChannels(t *testing.T) {
	s, now := maintenanceTestStore(t)
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "All services", EndAt: *now + 60, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertChannel(Channel{ID: "new-game", Name: "New Game", Type: "mock"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MaintenanceForProbe("new-game", *now, *now)
	if err != nil || got == nil || got.ID != m.ID {
		t.Fatal("all-channels window did not include new service", got, err)
	}
}

func TestMaintenanceInputValidation(t *testing.T) {
	s, now := maintenanceTestStore(t)
	for _, in := range []MaintenanceInput{
		{EndAt: *now + 60, AllChannels: true},
		{Title: "Missing scope", EndAt: *now + 60},
		{Title: "Both scopes", EndAt: *now + 60, AllChannels: true, ChannelIDs: []string{"game"}},
		{Title: "Missing channel", EndAt: *now + 60, ChannelIDs: []string{"absent"}},
		{Title: "Duplicate", EndAt: *now + 60, ChannelIDs: []string{"game", "game"}},
		{Title: "Bad ID", EndAt: *now + 60, ChannelIDs: []string{"<script>"}},
		{Title: "Past", StartAt: *now - 100, EndAt: *now + 60, AllChannels: true},
		{Title: "Reverse", StartAt: *now + 100, EndAt: *now + 50, AllChannels: true},
		{Title: "Long", EndAt: *now + 31*daySeconds, AllChannels: true},
		{Title: "Distant", StartAt: *now + 366*daySeconds, EndAt: *now + 366*daySeconds + 10, AllChannels: true},
	} {
		if _, err := s.CreateMaintenance(in); !errors.Is(err, ErrMaintenanceInput) {
			t.Fatalf("input %+v accepted or wrong error: %v", in, err)
		}
	}
	list, err := s.ListMaintenance(false, 200)
	if err != nil || len(list) != 0 {
		t.Fatal("invalid requests left partial data", list, err)
	}
	if _, err := s.CreateMaintenance(MaintenanceInput{Title: "Maximum", EndAt: *now + 30*daySeconds, AllChannels: true}); err != nil {
		t.Fatal("30-day boundary should work", err)
	}
}

func TestMaintenancePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Restart", EndAt: time.Now().Unix() + 3600, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetMaintenance(m.ID)
	if err != nil || got.State != "active" {
		t.Fatal("maintenance lost after restart", got, err)
	}
}

func TestMaintenanceStatusKeepsProbeEvidence(t *testing.T) {
	s, now := maintenanceTestStore(t)
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Planned restart", EndAt: *now + 60, ChannelIDs: []string{"game"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.addProbeWithMaintenance("game", ProbeResult{Error: "maintenance outage"}, &m.ID); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(27000)
	if err != nil {
		t.Fatal(err)
	}
	var game ChannelStatus
	for _, ch := range snap.Channels {
		if ch.ID == "game" {
			game = ch
		}
	}
	if game.CurrentStatus != "partial" || game.Maintenance == nil || game.Availability == nil || *game.Availability != 0 || game.Probes != 1 {
		t.Fatalf("maintenance falsified status: %+v", game)
	}
	if game.Buckets[len(game.Buckets)-1].Status != "down" {
		t.Fatal("raw failure bucket must remain down")
	}
	history, err := s.History(1)
	if err != nil {
		t.Fatal(err)
	}
	if history.Channels[0].Days[0].Probes != 1 {
		t.Fatal("maintenance result missing from history")
	}
	*now = m.EndAt
	snap, err = s.Snapshot(27000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Channels[0].Maintenance != nil || snap.Channels[0].CurrentStatus != "down" {
		t.Fatal("maintenance did not expire into real probe status")
	}
}

func TestMaintenanceDynamicsAndCancellationHistory(t *testing.T) {
	s, now := maintenanceTestStore(t)
	m, err := s.CreateMaintenance(MaintenanceInput{Title: "Planned", Body: "Restarting game", StartAt: *now + 60, EndAt: *now + 120, AllChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDynamics(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "m1" || list[0].State != "pending" || list[0].Source != "scheduled" {
		t.Fatal("missing scheduled dynamic", list)
	}
	d, err := s.GetDynamics("m1")
	if err != nil || d.ID != "m1" {
		t.Fatal(d, err)
	}
	if err := s.CancelMaintenance(m.ID); err != nil {
		t.Fatal(err)
	}
	d, err = s.GetDynamics("m1")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "resolved" || len(d.Updates) != 1 || d.Updates[0].Label != "维护计划已取消" {
		t.Fatal("cancel history missing", d)
	}
	if *d.EndAt < d.StartAt {
		t.Fatal("cancelled future dynamic must not have a reversed display time range")
	}
	if err := s.DeleteDynamic("m1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("maintenance must not be deleted through dynamic CRUD")
	}
}
