package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPct(t *testing.T) {
	if v := pct(1, 3); v == nil || *v != 33.33 {
		t.Fatalf("pct(1,3) = %v", v)
	}
	if v := pct(0, 0); v != nil {
		t.Fatalf("pct(0,0) = %v, want nil", v)
	}
	if v := pct(7, 7); v == nil || *v != 100 {
		t.Fatalf("pct(7,7) = %v", v)
	}
}

func TestRound1(t *testing.T) {
	if round1(1234.56) != 1234.6 {
		t.Fatalf("round1(1234.56) = %v", round1(1234.56))
	}
}

func TestSnapshotAndHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	enabled := true
	ch := Channel{ID: "c1", Name: "C1", Group: "G", Type: "mock", Enabled: &enabled}
	n, err := s.SeedChannels([]Channel{ch})
	if err != nil || n != 1 {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}

	ok := 1.0
	if err := s.AddProbe("c1", ProbeResult{OK: true, LatencyMs: &ok, TTFTMs: &ok, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProbe("c1", ProbeResult{OK: false, LatencyMs: &ok, Error: "boom"}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.Snapshot(3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Channels) != 1 {
		t.Fatalf("channels = %d", len(snap.Channels))
	}
	cs := snap.Channels[0]
	if cs.Probes != 2 {
		t.Fatalf("probes = %d", cs.Probes)
	}
	if cs.Availability == nil || *cs.Availability != 50 {
		t.Fatalf("availability = %v", cs.Availability)
	}
	withData := 0
	for _, b := range cs.Buckets {
		if b.Probes > 0 {
			withData++
			if b.Status != "partial" {
				t.Fatalf("bucket status = %s, want partial", b.Status)
			}
		}
	}
	if withData != 1 {
		t.Fatalf("buckets with data = %d, want 1", withData)
	}

	hist, err := s.History(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Channels) != 1 {
		t.Fatalf("history channels = %d", len(hist.Channels))
	}
	today := hist.Channels[0].Days[len(hist.Channels[0].Days)-1]
	if today.Probes != 2 || today.Availability == nil || *today.Availability != 50 {
		t.Fatalf("today = %+v", today)
	}
}

func TestIncidentFlow(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if in, _ := s.OpenIncidentFor("c1"); in != nil {
		t.Fatalf("unexpected open incident: %+v", in)
	}
	if _, err := s.OpenIncident("c1", "G", "test failure"); err != nil {
		t.Fatal(err)
	}
	in, err := s.OpenIncidentFor("c1")
	if err != nil || in == nil {
		t.Fatalf("open incident: %v %v", in, err)
	}
	if err := s.ResolveIncident("c1"); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.OpenIncidentFor("c1"); in != nil {
		t.Fatalf("incident not resolved: %+v", in)
	}
}

func TestValidateChannel(t *testing.T) {
	cases := []struct {
		ch    Channel
		okErr bool
	}{
		{Channel{ID: "a", Name: "A", Type: "mock"}, false},
		{Channel{ID: "a", Name: "A", Type: "openai"}, true},
		{Channel{ID: "a", Name: "A", Type: "openai",
			Config: map[string]any{"base_url": "https://x/v1", "model": "m"}}, false},
		{Channel{ID: "a", Name: "A", Type: "http"}, true},
		{Channel{ID: "a", Name: "A", Type: "http",
			Config: map[string]any{"url": "https://x"}}, false},
		{Channel{ID: "a", Name: "A", Type: "weird"}, true},
		{Channel{ID: "", Name: "A", Type: "mock"}, true},
	}
	for i, c := range cases {
		err := validateChannel(c.ch)
		if c.okErr && err == nil {
			t.Fatalf("case %d: expected error, got nil", i)
		}
		if !c.okErr && err != nil {
			t.Fatalf("case %d: unexpected error: %v", i, err)
		}
	}
}

func TestSeedOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ch := Channel{ID: "c1", Name: "C1", Type: "mock"}
	if n, err := s.SeedChannels([]Channel{ch}); err != nil || n != 1 {
		t.Fatalf("first seed: n=%d err=%v", n, err)
	}
	if err := s.DeleteChannel("c1"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedChannels([]Channel{ch}); err != nil || n != 0 {
		t.Fatalf("second seed must not resurrect channel: n=%d err=%v", n, err)
	}
	chs, err := s.ListChannels(false)
	if err != nil || len(chs) != 0 {
		t.Fatalf("channels after reseed = %v, %v", chs, err)
	}
}

func TestManualIncidentSurvivesAutoProbe(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.OpenManualIncident("c1", "G", "manual maintenance")
	if err != nil {
		t.Fatal(err)
	}
	// 自动恢复逻辑只关闭 auto 事件，手动事件必须保留。
	if err := s.ResolveIncident("c1"); err != nil {
		t.Fatal(err)
	}
	incs, err := s.Incidents(10)
	if err != nil || len(incs) != 1 {
		t.Fatalf("incidents = %v, %v", incs, err)
	}
	if incs[0].Source != "manual" || incs[0].ResolvedAt != nil {
		t.Fatalf("manual incident changed: %+v", incs[0])
	}
	if err := s.ResolveIncidentByID(id); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveIncidentByID(id); err == nil {
		t.Fatal("resolving a closed incident must fail")
	}
}

func TestHistorySurvivesRetentionCleanup(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 模拟 40 天前的一条探测：原始明细会被清理，日汇总应保留。
	old := float64(100)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	oldTS := s.now().AddDate(0, 0, -40).Unix()
	if _, err := tx.Exec(`INSERT INTO probes (channel_id, ts, ok, latency_ms) VALUES ('c1', ?, 1, ?)`, oldTS, old); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProbe("c1", ProbeResult{OK: true, LatencyMs: &old, TTFTMs: &old, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(30); err != nil {
		t.Fatal(err)
	}
	var raw int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM probes WHERE ts < ?`, s.now().Unix()-30*86400).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 0 {
		t.Fatalf("old raw probes not cleaned: %d", raw)
	}
	hist, err := s.History(90)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Channels) != 0 {
		t.Fatalf("history includes disabled/unknown channels: %+v", hist.Channels)
	}
}

func TestCurrentStatusAndStaleness(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	enabled := true
	if _, err := s.SeedChannels([]Channel{{ID: "c1", Name: "C1", Type: "mock", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	s.SetProbeTiming(60, 30)
	lat := float64(10)
	if err := s.AddProbe("c1", ProbeResult{OK: true, LatencyMs: &lat, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(27000)
	if err != nil {
		t.Fatal(err)
	}
	cs := snap.Channels[0]
	if cs.CurrentStatus != "ok" || cs.LastProbeAt == nil {
		t.Fatalf("current status = %s last = %v", cs.CurrentStatus, cs.LastProbeAt)
	}
	if cs.AvgTTFTMs != nil {
		t.Fatalf("http-ish channel should not report ttft: %v", cs.AvgTTFTMs)
	}
	if len(cs.Buckets) != 90 {
		t.Fatalf("buckets = %d, want 90", len(cs.Buckets))
	}
	// 时间大幅推进后视为过期。
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	snap, err = s.Snapshot(27000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Channels[0].CurrentStatus != "nodata" {
		t.Fatalf("stale channel status = %s", snap.Channels[0].CurrentStatus)
	}
}

func TestSeedSortStartsAtOne(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chs := []Channel{
		{ID: "a", Name: "A", Type: "mock"},
		{ID: "b", Name: "B", Type: "mock"},
		{ID: "c", Name: "C", Type: "mock"},
	}
	if _, err := s.SeedChannels(chs); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT id, sort FROM channels ORDER BY sort`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]int{"a": 1, "b": 2, "c": 3}
	for rows.Next() {
		var id string
		var sort int
		if err := rows.Scan(&id, &sort); err != nil {
			t.Fatal(err)
		}
		if want[id] != sort {
			t.Fatalf("channel %s sort = %d, want %d", id, sort, want[id])
		}
	}
}

func TestLegacyZeroSortMigratesLast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := s.SeedChannels([]Channel{{ID: "new", Name: "N", Type: "mock", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	// 模拟升级前直接写库的 sort=0 渠道
	if _, err := s.db.Exec(`INSERT INTO channels (id, name, group_name, type, config, enabled, sort)
		VALUES ('legacy', 'L', 'G', 'mock', '{}', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	chs, err := s2.ListChannels(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 || chs[0].ID != "new" || chs[1].ID != "legacy" {
		t.Fatalf("order = %v,%v; legacy zero-sort must move last", chs[0].ID, chs[1].ID)
	}
}

func TestBucketPlayersAverage(t *testing.T) {
	s := newTestStore(t)
	now := s.now().Unix()
	base := (now / bucketSeconds) * bucketSeconds
	enabled := true
	if _, err := s.SeedChannels([]Channel{{ID: "mc", Name: "MC", Type: "minecraft", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	insert := func(ts int64, players *int) {
		t.Helper()
		var max *int = players
		if _, err := s.db.Exec(
			`INSERT INTO probes (channel_id, ts, ok, latency_ms, players, max_players) VALUES ('mc', ?, 1, 10, ?, ?)`,
			ts, players, max); err != nil {
			t.Fatal(err)
		}
	}
	five, seven, three := 5, 7, 3
	insert(base+1, &five)
	insert(base+2, &seven)
	insert(base+3, nil) // failed probes without player data must not pull the average down
	insert(base-bucketSeconds+1, &three)

	snap, err := s.Snapshot(600)
	if err != nil {
		t.Fatal(err)
	}
	cs := snap.Channels[0]
	if len(cs.Buckets) != 2 {
		t.Fatalf("buckets = %d", len(cs.Buckets))
	}
	previous := cs.Buckets[0].PlayersAvg
	if previous == nil || *previous != 3 {
		t.Fatalf("previous bucket average = %v, want 3", previous)
	}
	current := cs.Buckets[1].PlayersAvg
	if current == nil || *current != 6 {
		t.Fatalf("current bucket average = %v, want 6", current)
	}
	// A channel without player data must not expose the field.
	if _, err := s.SeedChannels([]Channel{{ID: "web", Name: "Web", Type: "http", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	snap, err = s.Snapshot(600)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range snap.Channels {
		if ch.ID == "web" {
			for _, b := range ch.Buckets {
				if b.PlayersAvg != nil {
					t.Fatal("channel without player probes reported players_avg")
				}
			}
		}
	}
}

func TestDisablingChannelResolvesOpenIncident(t *testing.T) {
	s := newTestStore(t)
	ch := Channel{ID: "game", Name: "Game", Group: "游戏", Type: "mock", Config: map[string]any{}}
	if err := s.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenIncident("game", "游戏", "connection failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenManualIncident("game", "游戏", "手动维护记录"); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenIncidentFor("game")
	if err != nil || open == nil {
		t.Fatalf("expected an open automatic incident: %+v %v", open, err)
	}

	// A disabled channel is no longer probed, so its automatic incident must not
	// stay open forever on the public timeline.
	disabled := false
	ch.Enabled = &disabled
	if err := s.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenIncidentFor("game"); err != nil || open != nil {
		t.Fatalf("automatic incident still open after disable: %+v %v", open, err)
	}
	// Manual incidents are operator records and must survive unrelated changes.
	var manualOpen int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM incidents WHERE channel_id = 'game' AND resolved_at IS NULL AND source = 'manual'`).Scan(&manualOpen); err != nil {
		t.Fatal(err)
	}
	if manualOpen != 1 {
		t.Fatalf("manual incident count = %d, want 1", manualOpen)
	}

	// Re-enabling must not resurrect the resolved incident.
	enabled := true
	ch.Enabled = &enabled
	if err := s.UpsertChannel(ch); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenIncidentFor("game"); err != nil || open != nil {
		t.Fatalf("resolved incident reopened on enable: %+v %v", open, err)
	}
}
