package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMaintenanceMigrationPreservesLegacyData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE channels(id TEXT PRIMARY KEY,name TEXT NOT NULL,group_name TEXT NOT NULL,type TEXT NOT NULL,config TEXT NOT NULL,enabled INTEGER NOT NULL,sort INTEGER NOT NULL)`,
		`INSERT INTO channels VALUES('game','Existing game','Games','mock','{}',1,4)`,
		`CREATE TABLE probes(id INTEGER PRIMARY KEY AUTOINCREMENT,channel_id TEXT NOT NULL,ts INTEGER NOT NULL,ok INTEGER NOT NULL,latency_ms REAL,ttft_ms REAL,status_code INTEGER,error TEXT,players INTEGER,max_players INTEGER)`,
		`INSERT INTO probes(channel_id,ts,ok,latency_ms,error,players,max_players) VALUES('game',1800000000,1,42,'',5,50)`,
		`CREATE TABLE incidents(id INTEGER PRIMARY KEY AUTOINCREMENT,channel_id TEXT NOT NULL,group_name TEXT NOT NULL,started_at INTEGER NOT NULL,resolved_at INTEGER,reason TEXT,source TEXT NOT NULL DEFAULT 'auto')`,
		`INSERT INTO incidents(channel_id,group_name,started_at,reason,source) VALUES('game','Games',1799990000,'existing manual event','manual')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chs, err := s.ListChannels(false)
	if err != nil || len(chs) != 1 || chs[0].Sort != 4 {
		t.Fatal("legacy channel changed", chs, err)
	}
	var count, players int
	var maintenanceID sql.NullInt64
	if err := s.db.QueryRow(`SELECT COUNT(*),players,maintenance_id FROM probes`).Scan(&count, &players, &maintenanceID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || players != 5 || maintenanceID.Valid {
		t.Fatal("legacy probe changed")
	}
	incs, err := s.Incidents(10)
	if err != nil || len(incs) != 1 || incs[0].Source != "manual" || incs[0].ResolvedAt != nil {
		t.Fatal("legacy incident changed", incs, err)
	}
	if windows, err := s.ListMaintenance(false, 200); err != nil || len(windows) != 0 {
		t.Fatal("migration created a real maintenance window", windows, err)
	}
	if _, err := s.SeedChannels([]Channel{{ID: "new-seed", Name: "new", Type: "mock"}}); err != nil {
		t.Fatal(err)
	}
	chs, err = s.ListChannels(false)
	if err != nil || len(chs) != 1 {
		t.Fatal("migration re-seeded old installation")
	}
}
