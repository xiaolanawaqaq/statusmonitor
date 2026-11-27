package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	bucketSeconds int64 = 300
	daySeconds    int64 = 86400
)

type Store struct {
	db              *sql.DB
	timingMu        sync.RWMutex
	maintenanceMu   sync.RWMutex
	intervalSeconds int
	timeoutSeconds  int
	now             func() time.Time
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = "data/status.db"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)", path)
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	s := &Store{db: database, intervalSeconds: 60, timeoutSeconds: 30, now: time.Now}
	if err := s.migrate(); err != nil {
		database.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SetProbeTiming(intervalSeconds, timeoutSeconds int) {
	if intervalSeconds <= 0 {
		intervalSeconds = 60
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	s.timingMu.Lock()
	s.intervalSeconds, s.timeoutSeconds = intervalSeconds, timeoutSeconds
	s.timingMu.Unlock()
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS channels (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL,
		group_name  TEXT NOT NULL,
		type        TEXT NOT NULL,
		config      TEXT NOT NULL DEFAULT '{}',
		enabled     INTEGER NOT NULL DEFAULT 1,
		sort        INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS probes (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id  TEXT NOT NULL,
		ts          INTEGER NOT NULL,
		ok          INTEGER NOT NULL,
		latency_ms  REAL,
		ttft_ms     REAL,
		status_code INTEGER,
		error       TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_probes_channel_ts ON probes(channel_id, ts)`,
	`CREATE INDEX IF NOT EXISTS idx_probes_ts ON probes(ts)`,
	`CREATE TABLE IF NOT EXISTS daily_probe_stats (
		channel_id TEXT NOT NULL,
		day_ts     INTEGER NOT NULL,
		probes     INTEGER NOT NULL DEFAULT 0,
		ok_probes  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (channel_id, day_ts)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_daily_probe_stats_day ON daily_probe_stats(day_ts)`,
	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS announcements (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		ts      INTEGER NOT NULL,
		title   TEXT NOT NULL,
		body    TEXT NOT NULL DEFAULT '',
		level   TEXT NOT NULL DEFAULT 'info',
		pinned  INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS incidents (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id  TEXT NOT NULL,
		group_name  TEXT NOT NULL,
		started_at  INTEGER NOT NULL,
		resolved_at INTEGER,
		reason      TEXT,
		source      TEXT NOT NULL DEFAULT 'auto'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_incidents_open ON incidents(channel_id, resolved_at)`,
}

func (s *Store) migrate() error {
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	// 旧库的 incidents 没有 source 列，补上后再统一按 auto 处理。
	if err := s.ensureColumn("incidents", "source", "TEXT NOT NULL DEFAULT 'auto'"); err != nil {
		return err
	}
	// 历史 sort=0 的行统一挪到最后（0 重新定义为"未设置"）。
	if _, err := s.db.Exec(`UPDATE channels SET sort = 1000 WHERE sort = 0`); err != nil {
		return err
	}
	// 游戏服探测的玩家数。
	if err := s.ensureColumn("probes", "players", "INTEGER"); err != nil {
		return err
	}
	if err := s.ensureColumn("probes", "max_players", "INTEGER"); err != nil {
		return err
	}
	// 配置错误（缺少 url/key 等）不代表服务故障，需要和历史数据区分。
	if err := s.ensureColumn("probes", "config_error", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.migrateDynamics(); err != nil {
		return err
	}
	if err := s.migrateMaintenance(); err != nil {
		return err
	}
	// 旧数据把"缺少 url/key 等配置问题"记成了失败。一次性标记出来，
	// 否则升级后这些渠道的 90 天可用率仍然显示为 0%。
	if err := s.migrateConfigErrors(); err != nil {
		return err
	}
	// 同一条配置错误可能已经堆积了成千上万行，只保留每个渠道最新的一条。
	if err := s.pruneDuplicateConfigErrors(); err != nil {
		return err
	}
	// 首次升级时用原始明细回填日汇总；已有汇总不覆盖。
	if _, err := s.db.Exec(
		`INSERT INTO daily_probe_stats (channel_id, day_ts, probes, ok_probes)
		 SELECT channel_id, (ts / ?) * ? AS day_ts, COUNT(*), SUM(ok)
		 FROM probes GROUP BY channel_id, day_ts
		 ON CONFLICT(channel_id, day_ts) DO NOTHING`,
		daySeconds, daySeconds); err != nil {
		return err
	}
	return nil
}

// migrateConfigErrors backfills the config_error flag for rows written before it existed.
func (s *Store) migrateConfigErrors() error {
	var done string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'config_error_backfill'`).Scan(&done)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, msg := range []string{
		"missing url", "invalid HTTP request", "missing API key", "missing base_url", "missing model",
		"invalid OpenAI request", "tcp channel requires host and port", "minecraft channel requires host and port",
		"steam channel requires host and port", "udp channel requires host, port and send",
		"icmp channel requires host", "dns channel requires host", "unknown channel type",
	} {
		if _, err := tx.Exec(
			`UPDATE probes SET config_error = 1 WHERE ok = 0 AND error = ?`, msg); err != nil {
			return err
		}
	}
	// 这些行之前被计入了日汇总，现在减掉，保持历史可用率不因配置问题失真。
	if _, err := tx.Exec(
		`UPDATE daily_probe_stats SET probes = MAX(0, probes - (
		   SELECT COUNT(*) FROM probes p
		   WHERE p.channel_id = daily_probe_stats.channel_id
		     AND (p.ts / ?) * ? = daily_probe_stats.day_ts
		     AND p.config_error = 1))`,
		daySeconds, daySeconds); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('config_error_backfill', '1')`); err != nil {
		return err
	}
	return tx.Commit()
}

// pruneDuplicateConfigErrors keeps one row per channel and error message.
func (s *Store) pruneDuplicateConfigErrors() error {
	var done string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'config_error_dedupe'`).Scan(&done)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM probes WHERE config_error = 1 AND id NOT IN (
		   SELECT MAX(id) FROM probes WHERE config_error = 1 GROUP BY channel_id, error)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('config_error_dedupe', '1')`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err
}

// ---------- channels ----------

func (s *Store) SeedChannels(chs []Channel) (int, error) {
	// 只在首次初始化时从 config 导入渠道；之后以后台/数据库为准，
	// 避免后台删除的渠道在重启后又冒出来。
	var seeded string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'channels_seeded'`).Scan(&seeded)
	if err == nil {
		return 0, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	var existing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM channels`).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		_, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('channels_seeded', '1')`)
		return 0, err
	}
	added := 0
	for i, ch := range chs {
		enabled := 0
		if ch.IsEnabled() {
			enabled = 1
		}
		sort := ch.Sort
		if sort == 0 {
			sort = i + 1
		}
		res, err := s.db.Exec(
			`INSERT OR IGNORE INTO channels (id, name, group_name, type, config, enabled, sort)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ch.ID, ch.Name, groupOr(ch.Group), ch.Type, ch.ConfigJSON(), enabled, sort,
		)
		if err != nil {
			return added, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('channels_seeded', '1')`); err != nil {
		return added, err
	}
	return added, nil
}

func (s *Store) ListChannels(enabledOnly bool) ([]Channel, error) {
	q := `SELECT id, name, group_name, type, config, enabled, sort FROM channels`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY sort, id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Channel{}
	for rows.Next() {
		var ch Channel
		var cfgStr string
		var enabled int
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Group, &ch.Type, &cfgStr, &enabled, &ch.Sort); err != nil {
			return nil, err
		}
		ch.Config = map[string]any{}
		if cfgStr != "" {
			_ = json.Unmarshal([]byte(cfgStr), &ch.Config)
		}
		b := enabled == 1
		ch.Enabled = &b
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (s *Store) UpsertChannel(ch Channel) error {
	enabled := 0
	if ch.IsEnabled() {
		enabled = 1
	}
	// 显式传了 sort 就用新值；未传时新渠道排最后（1000），老渠道保留原排序。
	var sort int
	existed := false
	if err := s.db.QueryRow(`SELECT sort FROM channels WHERE id = ?`, ch.ID).Scan(&sort); err == nil {
		existed = true
	} else if err != sql.ErrNoRows {
		return err
	}
	switch {
	case ch.Sort > 0:
		sort = ch.Sort
	case !existed:
		sort = 1000
	}
	_, err := s.db.Exec(
		`INSERT INTO channels (id, name, group_name, type, config, enabled, sort)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   name = excluded.name,
		   group_name = excluded.group_name,
		   type = excluded.type,
		   config = excluded.config,
		   enabled = excluded.enabled,
		   sort = excluded.sort`,
		ch.ID, ch.Name, groupOr(ch.Group), ch.Type, ch.ConfigJSON(), enabled, sort,
	)
	if err != nil {
		return err
	}
	if enabled == 0 {
		// 停用后不再探测，未关闭的自动故障事件不会再有恢复探测来结束它。
		return s.ResolveIncident(ch.ID)
	}
	return nil
}

func (s *Store) DeleteChannel(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM channels WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE incidents SET resolved_at = ? WHERE channel_id = ? AND resolved_at IS NULL`,
		time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- probes ----------

func (s *Store) AddProbe(channelID string, r ProbeResult) error {
	return s.addProbeWithMaintenance(channelID, r, nil)
}

func (s *Store) addProbeWithMaintenance(channelID string, r ProbeResult, maintenanceID *int64) error {
	okInt := 0
	if r.OK {
		okInt = 1
	}
	now := s.now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 配置错误每次都完全相同：只保留一条并刷新时间，避免每次探测都堆一行。
	if r.ConfigError && maintenanceID == nil {
		res, err := tx.Exec(
			`UPDATE probes SET ts = ? WHERE id = (
			   SELECT id FROM probes WHERE channel_id = ? AND config_error = 1 AND error = ?
			   ORDER BY id DESC LIMIT 1)`,
			now, channelID, r.Error)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			return tx.Commit()
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO probes (channel_id, ts, ok, latency_ms, ttft_ms, status_code, error, players, max_players, config_error, maintenance_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		channelID, now, okInt, r.LatencyMs, r.TTFTMs, r.StatusCode, r.Error, r.Players, r.MaxPlayers, boolInt(r.ConfigError), maintenanceID,
	); err != nil {
		return err
	}
	// 配置错误不进日汇总，否则 90 天历史会把未配置的渠道算成 0% 可用。
	if !r.ConfigError {
		if _, err := tx.Exec(
			`INSERT INTO daily_probe_stats (channel_id, day_ts, probes, ok_probes)
			 VALUES (?, ?, 1, ?)
			 ON CONFLICT(channel_id, day_ts) DO UPDATE SET
			   probes = probes + 1,
			   ok_probes = ok_probes + excluded.ok_probes`,
			channelID, (now/daySeconds)*daySeconds, okInt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProbeHealth is the latest stored probe outcome for one channel.
type ProbeHealth struct {
	TS          int64
	OK          bool
	ConfigError bool
	Error       string
}

// LatestProbeHealth returns the newest probe result per channel, for operator views.
func (s *Store) LatestProbeHealth() (map[string]ProbeHealth, error) {
	rows, err := s.db.Query(
		`SELECT p.channel_id, p.ts, p.ok, p.config_error, COALESCE(p.error, '')
		 FROM probes p
		 JOIN (SELECT channel_id, MAX(id) AS id FROM probes GROUP BY channel_id) m ON p.id = m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProbeHealth{}
	for rows.Next() {
		var cid string
		var h ProbeHealth
		var ok, configError int
		if err := rows.Scan(&cid, &h.TS, &ok, &configError, &h.Error); err != nil {
			return nil, err
		}
		h.OK = ok == 1
		h.ConfigError = configError == 1
		out[cid] = h
	}
	return out, rows.Err()
}

// ProbeFreshness returns the newest probe timestamp and how many channels are enabled.
func (s *Store) ProbeFreshness() (newest int64, enabled int, err error) {
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(ts), 0) FROM probes`).Scan(&newest); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE enabled = 1`).Scan(&enabled); err != nil {
		return 0, 0, err
	}
	return newest, enabled, nil
}

// LastProbeLatency returns the latency of the newest successful probe, or nil when none exists.
func (s *Store) LastProbeLatency(channelID string) (*float64, error) {
	var lat sql.NullFloat64
	err := s.db.QueryRow(`SELECT latency_ms FROM probes
	 WHERE channel_id = ? AND ok = 1 AND latency_ms IS NOT NULL
	 ORDER BY ts DESC, id DESC LIMIT 1`, channelID).Scan(&lat)
	if err == sql.ErrNoRows || !lat.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lat.Float64, nil
}

// ProbeStreak returns the trailing consecutive failures/successes from raw probes.
func (s *Store) ProbeStreak(channelID string) (fails, oks int, err error) {
	return s.probeStreakSince(channelID, 0)
}

func (s *Store) probeStreakSince(channelID string, since int64) (fails, oks int, err error) {
	rows, err := s.db.Query(
		`SELECT ok, maintenance_id FROM probes
		 WHERE channel_id = ? AND ts >= ? AND config_error = 0
		 ORDER BY ts DESC, id DESC LIMIT 1000`, channelID, since)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var first *int
	for rows.Next() {
		var ok int
		var maintenanceID sql.NullInt64
		if err := rows.Scan(&ok, &maintenanceID); err != nil {
			return 0, 0, err
		}
		if maintenanceID.Valid {
			break
		}
		if first == nil {
			first = &ok
		}
		if ok != *first {
			break
		}
		if ok == 1 {
			oks++
		} else {
			fails++
		}
	}
	return fails, oks, rows.Err()
}

func (s *Store) Cleanup(retentionDays int) error {
	cutoff := s.now().Unix() - int64(retentionDays)*86400
	if _, err := s.db.Exec(`DELETE FROM probes WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	// 日汇总保留至少 90 天，保证 90 天历史页在明细清理后仍有数据。
	summaryCutoff := s.now().Unix() - 90*daySeconds
	if _, err := s.db.Exec(`DELETE FROM daily_probe_stats WHERE day_ts < ?`, summaryCutoff); err != nil {
		return err
	}
	incidentCutoff := s.now().Unix() - 90*daySeconds
	_, err := s.db.Exec(
		`DELETE FROM incidents WHERE resolved_at IS NOT NULL AND resolved_at < ?`, incidentCutoff)
	return err
}

// ---------- status snapshot ----------

type Bucket struct {
	TS              int64    `json:"ts"`
	Status          string   `json:"status"`
	Probes          int      `json:"probes"`
	OkProbes        int      `json:"ok_probes"`
	LatencyMs       *float64 `json:"latency_ms"`
	TTFTMs          *float64 `json:"ttft_ms"`
	PlayersAvg      *float64 `json:"players_avg"`
	At              int64    `json:"at"`
	DurationSeconds int      `json:"duration_seconds"`
}

type ChannelStatus struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Group         string             `json:"group"`
	Type          string             `json:"type"`
	CurrentStatus string             `json:"current_status"`
	LastProbeAt   *int64             `json:"last_probe_at"`
	Players       *int               `json:"players,omitempty"`
	MaxPlayers    *int               `json:"max_players,omitempty"`
	Availability  *float64           `json:"availability"`
	Probes        int                `json:"probes"`
	AvgLatencyMs  *float64           `json:"avg_latency_ms"`
	AvgTTFTMs     *float64           `json:"avg_ttft_ms"`
	Buckets       []Bucket           `json:"buckets"`
	Incident      *Incident          `json:"incident"`
	Maintenance   *MaintenanceWindow `json:"maintenance"`
}

type SiteInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

type StatusResponse struct {
	GeneratedAt   int64               `json:"generated_at"`
	WindowSeconds int                 `json:"window_seconds"`
	BucketSeconds int                 `json:"bucket_seconds"`
	Channels      []ChannelStatus     `json:"channels"`
	Site          SiteInfo            `json:"site"`
	Maintenance   []MaintenanceWindow `json:"maintenance"`
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func pct(part, total int) *float64 {
	if total == 0 {
		return nil
	}
	v := math.Round(float64(part)/float64(total)*10000) / 100
	return &v
}

type probeRow struct {
	ChannelID string
	TS        int64
	OK        int
	Latency   *float64
	TTFT      *float64
}

func (s *Store) Snapshot(windowSeconds int) (*StatusResponse, error) {
	now := s.now().Unix()
	count := int(int64(windowSeconds) / bucketSeconds)
	if count < 1 {
		count = 1
	}
	current := (now / bucketSeconds) * bucketSeconds
	first := current - int64(count-1)*bucketSeconds

	// 按 5 分钟桶在 SQL 中聚合，避免把窗口内全部原始行拉进内存。
	rows, err := s.db.Query(
		`SELECT channel_id, (ts / ?) * ? AS bucket, COUNT(*) AS probes,
		        SUM(ok) AS ok_probes,
		        SUM(config_error) AS config_errors,
		        SUM(CASE WHEN ok = 1 AND latency_ms IS NOT NULL THEN latency_ms END) AS lat_sum,
		        COUNT(CASE WHEN ok = 1 AND latency_ms IS NOT NULL THEN 1 END) AS lat_n,
		        SUM(CASE WHEN ok = 1 AND ttft_ms IS NOT NULL THEN ttft_ms END) AS ttft_sum,
	        COUNT(CASE WHEN ok = 1 AND ttft_ms IS NOT NULL THEN 1 END) AS ttft_n,
	        SUM(CASE WHEN players IS NOT NULL THEN players END) AS players_sum,
	        COUNT(players) AS players_n
	 FROM probes WHERE ts >= ?
	 GROUP BY channel_id, bucket`,
		bucketSeconds, bucketSeconds, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type aggRow struct {
		probes, okProbes, configErrors, latN, ttftN, playersN int
		latSum, ttftSum, playersSum                           sql.NullFloat64
	}
	agg := map[string]map[int64]aggRow{}
	for rows.Next() {
		var cid string
		var bucket int64
		var a aggRow
		if err := rows.Scan(&cid, &bucket, &a.probes, &a.okProbes, &a.configErrors, &a.latSum, &a.latN, &a.ttftSum, &a.ttftN, &a.playersSum, &a.playersN); err != nil {
			return nil, err
		}
		if agg[cid] == nil {
			agg[cid] = map[int64]aggRow{}
		}
		agg[cid][bucket] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type latestProbe struct {
		ts          int64
		ok          bool
		configError bool
		players     sql.NullInt64
		maxPlayers  sql.NullInt64
	}
	latest := map[string]latestProbe{}
	latestRows, err := s.db.Query(
		`SELECT p.channel_id, p.ts, p.ok, p.config_error, p.players, p.max_players FROM probes p
		 JOIN (SELECT channel_id, MAX(id) AS id FROM probes GROUP BY channel_id) m ON p.id = m.id`)
	if err != nil {
		return nil, err
	}
	for latestRows.Next() {
		var cid string
		var lp latestProbe
		var ok, configError int
		if err := latestRows.Scan(&cid, &lp.ts, &ok, &configError, &lp.players, &lp.maxPlayers); err != nil {
			latestRows.Close()
			return nil, err
		}
		lp.ok = ok == 1
		lp.configError = configError == 1
		latest[cid] = lp
	}
	if err := latestRows.Err(); err != nil {
		latestRows.Close()
		return nil, err
	}
	latestRows.Close()

	chans, err := s.ListChannels(true)
	if err != nil {
		return nil, err
	}
	open, err := s.openIncidentMap()
	if err != nil {
		return nil, err
	}
	maintenance, err := s.ListMaintenance(true, 200)
	if err != nil {
		return nil, err
	}
	activeMaintenance, err := s.ActiveMaintenanceByChannel(chans)
	if err != nil {
		return nil, err
	}

	s.timingMu.RLock()
	interval, timeout := s.intervalSeconds, s.timeoutSeconds
	s.timingMu.RUnlock()
	staleAfter := int64(max(2*interval+timeout, 60))

	resp := &StatusResponse{
		GeneratedAt:   now,
		WindowSeconds: windowSeconds,
		BucketSeconds: int(bucketSeconds),
		Channels:      []ChannelStatus{},
		Maintenance:   maintenance,
	}

	for _, ch := range chans {
		cs := ChannelStatus{
			ID: ch.ID, Name: ch.Name, Group: ch.Group, Type: ch.Type,
			Incident: open[ch.ID], Buckets: make([]Bucket, 0, count),
		}
		var total, okCount, latN, ttftN int
		var latSum, ttftSum float64

		for i := 0; i < count; i++ {
			bts := first + int64(i)*bucketSeconds
			a, has := agg[ch.ID][bts]
			// 配置错误不是服务健康信号：单独剔除，避免把未配置的渠道画成故障。
			real := 0
			if has {
				real = a.probes - a.configErrors
			}
			if !has || real <= 0 {
				cs.Buckets = append(cs.Buckets, Bucket{TS: bts, Status: "nodata"})
				continue
			}
			total += real
			okCount += a.okProbes
			latN += a.latN
			latSum += a.latSum.Float64
			// HTTP 探测没有"首字"概念；openai 与 mock(模拟大模型) 才统计 TTFT。
			if ch.Type != "http" {
				ttftN += a.ttftN
				ttftSum += a.ttftSum.Float64
			}
			status := "down"
			if a.okProbes == real {
				status = "ok"
			} else if a.okProbes > 0 {
				status = "partial"
			}
			var bLat *float64
			if a.latN > 0 {
				v := round1(a.latSum.Float64 / float64(a.latN))
				bLat = &v
			}
			var bTtft *float64
			// HTTP 探测没有"首字"概念；openai 与 mock(模拟大模型) 才统计 TTFT。
			if ch.Type != "http" && a.ttftN > 0 {
				v := round1(a.ttftSum.Float64 / float64(a.ttftN))
				bTtft = &v
			}
			var bPlayers *float64
			if a.playersN > 0 {
				v := round1(a.playersSum.Float64 / float64(a.playersN))
				bPlayers = &v
			}
			cs.Buckets = append(cs.Buckets, Bucket{
				TS: bts, Status: status, Probes: a.probes, OkProbes: a.okProbes,
				LatencyMs: bLat, TTFTMs: bTtft, PlayersAvg: bPlayers,
				At: bts, DurationSeconds: int(bucketSeconds),
			})
		}

		cs.Probes = total
		if total > 0 {
			cs.Availability = pct(okCount, total)
		}
		if latN > 0 {
			v := round1(latSum / float64(latN))
			cs.AvgLatencyMs = &v
		}
		if ttftN > 0 {
			v := round1(ttftSum / float64(ttftN))
			cs.AvgTTFTMs = &v
		}

		status := "nodata"
		if lp, ok := latest[ch.ID]; ok {
			ts := lp.ts
			cs.LastProbeAt = &ts
			if lp.players.Valid {
				v := int(lp.players.Int64)
				cs.Players = &v
			}
			if lp.maxPlayers.Valid {
				v := int(lp.maxPlayers.Int64)
				cs.MaxPlayers = &v
			}
			if now-ts <= staleAfter {
				switch {
				case lp.ok:
					status = "ok"
				case lp.configError:
					// 未配置的渠道无法证明服务可用，但也不是故障，标成 nodata。
					status = "nodata"
				default:
					status = "down"
				}
			}
		}
		if in := open[ch.ID]; in != nil && in.Source == "manual" {
			status = "partial"
		}
		cs.Maintenance = activeMaintenance[ch.ID]
		if cs.Maintenance != nil {
			status = "partial"
		}
		cs.CurrentStatus = status

		resp.Channels = append(resp.Channels, cs)
	}
	return resp, nil
}

// ---------- history ----------

type DayStat struct {
	TS           int64    `json:"ts"`
	Probes       int      `json:"probes"`
	Availability *float64 `json:"availability"`
}

type ChannelHistory struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Group     string     `json:"group"`
	Type      string     `json:"type"`
	Days      []DayStat  `json:"days"`
	Incidents []Incident `json:"incidents"`
}

type HistoryResponse struct {
	GeneratedAt int64            `json:"generated_at"`
	Days        int              `json:"days"`
	Channels    []ChannelHistory `json:"channels"`
}

func (s *Store) History(days int) (*HistoryResponse, error) {
	now := s.now().Unix()
	dayStart := (now / daySeconds) * daySeconds
	firstDay := dayStart - int64(days-1)*daySeconds

	// 日汇总表保留 90 天，因此即使原始明细已清理，历史页仍然完整。
	rows, err := s.db.Query(
		`SELECT channel_id, day_ts, probes, ok_probes FROM daily_probe_stats WHERE day_ts >= ?`,
		firstDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agg := map[string]map[int64][2]int{}
	for rows.Next() {
		var cid string
		var dts int64
		var probes, okProbes int
		if err := rows.Scan(&cid, &dts, &probes, &okProbes); err != nil {
			return nil, err
		}
		if agg[cid] == nil {
			agg[cid] = map[int64][2]int{}
		}
		agg[cid][dts] = [2]int{probes, okProbes}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	chans, err := s.ListChannels(true)
	if err != nil {
		return nil, err
	}
	incs, err := s.incidentsSince(firstDay)
	if err != nil {
		return nil, err
	}
	byCh := map[string][]Incident{}
	for _, in := range incs {
		byCh[in.ChannelID] = append(byCh[in.ChannelID], in)
	}

	resp := &HistoryResponse{GeneratedAt: now, Days: days, Channels: []ChannelHistory{}}
	for _, ch := range chans {
		chh := ChannelHistory{
			ID: ch.ID, Name: ch.Name, Group: ch.Group, Type: ch.Type,
			Days: make([]DayStat, 0, days), Incidents: byCh[ch.ID],
		}
		if chh.Incidents == nil {
			chh.Incidents = []Incident{}
		}
		for i := days - 1; i >= 0; i-- {
			dts := dayStart - int64(i)*daySeconds
			v := agg[ch.ID][dts]
			ds := DayStat{TS: dts, Probes: v[0]}
			if v[0] > 0 {
				ds.Availability = pct(v[1], v[0])
			}
			chh.Days = append(chh.Days, ds)
		}
		resp.Channels = append(resp.Channels, chh)
	}
	return resp, nil
}

// ---------- announcements ----------

type Announcement struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Level  string `json:"level"`
	Pinned bool   `json:"pinned"`
}

func (s *Store) Announcements(limit int) ([]Announcement, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, title, body, level, pinned FROM announcements
		 ORDER BY pinned DESC, ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Announcement{}
	for rows.Next() {
		var a Announcement
		var pinned int
		if err := rows.Scan(&a.ID, &a.TS, &a.Title, &a.Body, &a.Level, &pinned); err != nil {
			return nil, err
		}
		a.Pinned = pinned == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddAnnouncement(title, body, level string, pinned bool) (int64, error) {
	pin := 0
	if pinned {
		pin = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO announcements (ts, title, body, level, pinned) VALUES (?, ?, ?, ?, ?)`,
		time.Now().Unix(), title, body, level, pin)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteAnnouncement(id int64) error {
	_, err := s.db.Exec(`DELETE FROM announcements WHERE id = ?`, id)
	return err
}

// ---------- incidents ----------

type Incident struct {
	ID         int64  `json:"id"`
	ChannelID  string `json:"channel_id"`
	Group      string `json:"group"`
	StartedAt  int64  `json:"started_at"`
	ResolvedAt *int64 `json:"resolved_at"`
	Reason     string `json:"reason"`
	Source     string `json:"source"`
}

type scanner interface {
	Scan(dest ...any) error
}

func scanIncident(sc scanner) (Incident, error) {
	var in Incident
	var resolved sql.NullInt64
	err := sc.Scan(&in.ID, &in.ChannelID, &in.Group, &in.StartedAt, &resolved, &in.Reason, &in.Source)
	if err != nil {
		return in, err
	}
	if in.Source == "" {
		in.Source = "auto"
	}
	if resolved.Valid {
		v := resolved.Int64
		in.ResolvedAt = &v
	}
	return in, nil
}

func (s *Store) OpenIncident(channelID, group, reason string) (int64, error) {
	return s.openIncident(channelID, group, reason, "auto")
}

func (s *Store) OpenManualIncident(channelID, group, reason string) (int64, error) {
	return s.openIncident(channelID, group, reason, "manual")
}

func (s *Store) openIncident(channelID, group, reason, source string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO incidents (channel_id, group_name, started_at, reason, source) VALUES (?, ?, ?, ?, ?)`,
		channelID, group, s.now().Unix(), reason, source)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResolveIncident 只关闭自动事件；手动事件必须由管理员显式关闭。
func (s *Store) ResolveIncident(channelID string) error {
	_, err := s.db.Exec(
		`UPDATE incidents SET resolved_at = ? WHERE channel_id = ? AND resolved_at IS NULL AND source = 'auto'`,
		s.now().Unix(), channelID)
	return err
}

func (s *Store) ResolveIncidentByID(id int64) error {
	res, err := s.db.Exec(
		`UPDATE incidents SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`,
		s.now().Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) OpenIncidentFor(channelID string) (*Incident, error) {
	row := s.db.QueryRow(
		`SELECT id, channel_id, group_name, started_at, resolved_at, reason, source
		 FROM incidents WHERE channel_id = ? AND resolved_at IS NULL AND source = 'auto'
		 ORDER BY id DESC LIMIT 1`, channelID)
	in, err := scanIncident(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *Store) openIncidentMap() (map[string]*Incident, error) {
	rows, err := s.db.Query(
		`SELECT id, channel_id, group_name, started_at, resolved_at, reason, source
		 FROM incidents WHERE resolved_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := map[string]*Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		cp := in
		m[in.ChannelID] = &cp
	}
	return m, rows.Err()
}

func (s *Store) incidentsSince(start int64) ([]Incident, error) {
	rows, err := s.db.Query(
		`SELECT id, channel_id, group_name, started_at, resolved_at, reason, source
		 FROM incidents WHERE started_at >= ? ORDER BY started_at DESC`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectIncidents(rows)
}

func (s *Store) Incidents(limit int) ([]Incident, error) {
	rows, err := s.db.Query(
		`SELECT id, channel_id, group_name, started_at, resolved_at, reason, source
		 FROM incidents ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectIncidents(rows)
}

func collectIncidents(rows *sql.Rows) ([]Incident, error) {
	out := []Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
