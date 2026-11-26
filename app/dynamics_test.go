package main

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDynamicLifecycle(t *testing.T) {
	s := newTestStore(t)

	id, err := s.AddDynamic(DynamicInput{
		Kind: KindIncident, Title: "数据库响应变慢", Body: "**影响**：登录",
		Services: []string{"登录"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "d1" {
		t.Fatalf("id = %q, want d1", id)
	}

	d, err := s.GetDynamics(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateActive {
		t.Fatalf("incident default state = %q, want active", d.State)
	}
	if len(d.Services) != 1 || d.Services[0] != "登录" {
		t.Fatalf("services = %v", d.Services)
	}

	if _, err := s.AddDynamicUpdate(id, "已定位", "回滚了慢查询", 0); err != nil {
		t.Fatal(err)
	}
	// 关闭事件时应自动补 end_at。
	if err := s.UpdateDynamic(id, map[string]any{"state": StateResolve}); err != nil {
		t.Fatal(err)
	}
	d, err = s.GetDynamics(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateResolve {
		t.Fatalf("state = %q", d.State)
	}
	if d.EndAt == nil {
		t.Fatal("resolved dynamic must have end_at")
	}
	if len(d.Updates) != 1 || d.Updates[0].Label != "已定位" {
		t.Fatalf("updates = %+v", d.Updates)
	}

	if err := s.DeleteDynamic(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDynamics(id); err == nil {
		t.Fatal("deleted dynamic still readable")
	}
}

func TestListDynamicsMergesIncidents(t *testing.T) {
	s := newTestStore(t)
	enabled := true
	if _, err := s.SeedChannels([]Channel{{ID: "c1", Name: "C1", Type: "mock", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDynamic(DynamicInput{Kind: KindNotice, Title: "计划维护", Pinned: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenIncident("c1", "G", "probe failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenManualIncident("c1", "G", "手动维护"); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListDynamics(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("dynamics = %d, want 3", len(list))
	}
	if list[0].Title != "计划维护" {
		t.Fatalf("pinned notice must sort first, got %q", list[0].Title)
	}
	manual := 0
	automatic := 0
	for _, d := range list {
		if d.Kind != KindIncident {
			continue
		}
		if d.Source == "manual" {
			manual++
		} else if d.Source == "auto" {
			automatic++
		}
	}
	if manual != 1 || automatic != 1 {
		t.Fatalf("incident sources manual=%d auto=%d", manual, automatic)
	}
}

func TestDynamicIDRejectsBadInput(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{"", "x1", "d", "d0", "d-1", "dabc", "1"} {
		if _, err := s.GetDynamics(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestMigrationFromAnnouncements(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧版本：有 announcements 数据但没迁移标记。
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key = 'dynamics_migrated'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM dynamics`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO announcements (ts, title, body, level, pinned) VALUES (?, ?, ?, 'info', 1)`,
		1700000000, "旧公告", "旧正文"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	list, err := s2.ListDynamics(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "旧公告" || !list[0].Pinned {
		t.Fatalf("migrated dynamics = %+v", list)
	}
}
