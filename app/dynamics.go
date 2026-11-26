package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Dynamic 是历史页展示的一条"动态"：公告、维护、故障事件。
// 自动探测产生的故障事件（incidents 表）在读取时合并进这个视图，id 前缀为 i，
// 人工发布的动态 id 前缀为 d，避免两张表的主键冲突。
type Dynamic struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	StartAt     int64           `json:"start_at"`
	EndAt       *int64          `json:"end_at"`
	Pinned      bool            `json:"pinned"`
	PinPosition int             `json:"pin_position"`
	Services    []string        `json:"services"`
	Updates     []DynamicUpdate `json:"updates"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
	Source      string          `json:"source"`
}

type DynamicUpdate struct {
	ID    int64  `json:"id"`
	At    int64  `json:"at"`
	Label string `json:"label"`
	Body  string `json:"body"`
}

const (
	KindNotice    = "notice"
	KindIncident  = "incident"
	KindMainten   = "maintenance"
	StatePublish  = "published"
	StateActive   = "active"
	StateResolve  = "resolved"
	dynamicPrefix = "d"
	incidentPref  = "i"
)

var dynamicKinds = map[string]bool{KindNotice: true, KindIncident: true, KindMainten: true}
var dynamicStates = map[string]bool{StatePublish: true, StateActive: true, StateResolve: true}

func validDynamicKind(k string) bool  { return dynamicKinds[k] }
func validDynamicState(s string) bool { return dynamicStates[s] }

// parseDynamicID 解析前端传来的 d12 / i3 形式 id。
func parseDynamicID(raw string) (kind byte, id int64, err error) {
	if len(raw) < 2 {
		return 0, 0, fmt.Errorf("invalid id")
	}
	n, convErr := strconv.ParseInt(raw[1:], 10, 64)
	if convErr != nil || n <= 0 {
		return 0, 0, fmt.Errorf("invalid id")
	}
	switch raw[0] {
	case 'd', 'i', 'm':
		return raw[0], n, nil
	}
	return 0, 0, fmt.Errorf("invalid id")
}

func dynamicID(n int64) string { return dynamicPrefix + strconv.FormatInt(n, 10) }

// numericDynamicID 把 "d1"/"i3" 映射成兼容层用的数字 id；
// 自动事件加 1_000_000 偏移，避免两张表的主键冲突。
func numericDynamicID(raw string) int64 {
	kind, id, err := parseDynamicID(raw)
	if err != nil {
		return 0
	}
	if kind == 'i' {
		return id + 1_000_000
	}
	if kind == 'm' {
		return id + 2_000_000
	}
	return id
}

// ---------- dynamics (人工发布) ----------

type DynamicInput struct {
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	StartAt     int64    `json:"start_at"`
	EndAt       *int64   `json:"end_at"`
	Pinned      bool     `json:"pinned"`
	PinPosition int      `json:"pin_position"`
	Services    []string `json:"services"`
}

func (in *DynamicInput) normalize(now int64) {
	if in.Kind == "" {
		in.Kind = KindNotice
	}
	if in.State == "" {
		if in.Kind == KindIncident && in.EndAt == nil {
			in.State = StateActive
		} else if in.Kind == KindIncident {
			in.State = StateResolve
		} else {
			in.State = StatePublish
		}
	}
	if in.Kind != KindIncident && in.EndAt == nil && in.State == StateResolve {
		t := now
		in.EndAt = &t
	}
	if in.StartAt <= 0 {
		in.StartAt = now
	}
	if in.Services == nil {
		in.Services = []string{}
	}
}

func (s *Store) AddDynamic(in DynamicInput) (string, error) {
	now := s.now().Unix()
	in.normalize(now)
	if !validDynamicKind(in.Kind) || !validDynamicState(in.State) {
		return "", fmt.Errorf("invalid kind or state")
	}
	services, _ := json.Marshal(in.Services)
	res, err := s.db.Exec(
		`INSERT INTO dynamics (kind, title, body, state, start_at, end_at, pinned, pin_position, services, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Kind, in.Title, in.Body, in.State, in.StartAt, in.EndAt,
		boolInt(in.Pinned), in.PinPosition, string(services), now, now)
	if err != nil {
		return "", err
	}
	id, err := res.LastInsertId()
	return dynamicID(id), err
}

func (s *Store) UpdateDynamic(rawID string, patch map[string]any) error {
	kind, id, err := parseDynamicID(rawID)
	if err != nil || kind != 'd' {
		return sql.ErrNoRows
	}
	sets := []string{"updated_at = ?"}
	args := []any{s.now().Unix()}
	if v, ok := patch["title"].(string); ok {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("title cannot be empty")
		}
		sets, args = append(sets, "title = ?"), append(args, v)
	}
	if v, ok := patch["body"].(string); ok {
		sets, args = append(sets, "body = ?"), append(args, v)
	}
	if v, ok := patch["state"].(string); ok {
		if !validDynamicState(v) {
			return fmt.Errorf("invalid state")
		}
		sets, args = append(sets, "state = ?"), append(args, v)
		if v == StateResolve {
			// 手动关闭时补上结束时间，除非显式传了 end_at。
			if _, has := patch["end_at"]; !has {
				sets, args = append(sets, "end_at = COALESCE(end_at, ?)"), append(args, s.now().Unix())
			}
		}
	}
	if v, ok := patch["end_at"]; ok {
		if v == nil {
			sets = append(sets, "end_at = NULL")
		} else if f, ok := v.(float64); ok {
			ts := int64(f)
			sets, args = append(sets, "end_at = ?"), append(args, ts)
		}
	}
	if v, ok := patch["pinned"].(bool); ok {
		sets, args = append(sets, "pinned = ?"), append(args, boolInt(v))
	}
	if v, ok := patch["pin_position"].(float64); ok {
		sets, args = append(sets, "pin_position = ?"), append(args, int(v))
	}
	if v, ok := patch["services"].([]any); ok {
		services := make([]string, 0, len(v))
		for _, item := range v {
			if sv, ok := item.(string); ok {
				services = append(services, sv)
			}
		}
		raw, _ := json.Marshal(services)
		sets, args = append(sets, "services = ?"), append(args, string(raw))
	}
	args = append(args, id)
	res, err := s.db.Exec(`UPDATE dynamics SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
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

func (s *Store) DeleteDynamic(rawID string) error {
	kind, id, err := parseDynamicID(rawID)
	if err != nil || kind != 'd' {
		return sql.ErrNoRows
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM dynamic_updates WHERE dynamic_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM dynamics WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) AddDynamicUpdate(rawID, label, body string, at int64) (int64, error) {
	kind, id, err := parseDynamicID(rawID)
	if err != nil || kind != 'd' {
		return 0, sql.ErrNoRows
	}
	if at <= 0 {
		at = s.now().Unix()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM dynamics WHERE id = ?`, id).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}
	res, err := tx.Exec(
		`INSERT INTO dynamic_updates (dynamic_id, at, label, body) VALUES (?, ?, ?, ?)`,
		id, at, label, body)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE dynamics SET updated_at = ? WHERE id = ?`, s.now().Unix(), id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func decodeServices(raw string) []string {
	out := []string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// ListDynamics 返回人工动态 + 自动/手动故障事件，按置顶与时间排序。
func (s *Store) ListDynamics(limit int) ([]Dynamic, error) {
	if limit <= 0 {
		limit = 200
	}
	dyRows, err := s.db.Query(
		`SELECT id, kind, title, body, state, start_at, end_at, pinned, pin_position,
		        services, created_at, updated_at
		 FROM dynamics ORDER BY start_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer dyRows.Close()

	out := []Dynamic{}
	byID := map[int64]int{}
	for dyRows.Next() {
		var d Dynamic
		var numID int64
		var end sql.NullInt64
		var pinned int
		var services string
		if err := dyRows.Scan(&numID, &d.Kind, &d.Title, &d.Body, &d.State, &d.StartAt, &end,
			&pinned, &d.PinPosition, &services, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.ID = dynamicID(numID)
		d.EndAt = nullInt(end)
		d.Pinned = pinned == 1
		d.Services = decodeServices(services)
		d.Updates = []DynamicUpdate{}
		d.Source = "human"
		byID[numID] = len(out)
		out = append(out, d)
	}
	if err := dyRows.Err(); err != nil {
		return nil, err
	}
	dyRows.Close()

	if err := s.attachUpdates(out, byID); err != nil {
		return nil, err
	}

	incs, err := s.Incidents(limit)
	if err != nil {
		return nil, err
	}
	for _, in := range incs {
		state := StateActive
		if in.ResolvedAt != nil {
			state = StateResolve
		}
		d := Dynamic{
			ID: incidentPref + strconv.FormatInt(in.ID, 10), Kind: KindIncident,
			Title: fmt.Sprintf("%s %s 故障", in.Group, in.ChannelID), Body: in.Reason,
			State: state, StartAt: in.StartedAt, EndAt: in.ResolvedAt,
			Services: []string{in.Group}, Updates: []DynamicUpdate{},
			CreatedAt: in.StartedAt, UpdatedAt: in.StartedAt, Source: in.Source,
		}
		if in.ResolvedAt != nil {
			d.UpdatedAt = *in.ResolvedAt
			d.Updates = append(d.Updates, DynamicUpdate{
				At: *in.ResolvedAt, Label: "已恢复", Body: "探测恢复正常，故障自动解除。",
			})
		}
		out = append(out, d)
	}

	windows, err := s.ListMaintenance(false, limit)
	if err != nil {
		return nil, err
	}
	for _, window := range windows {
		out = append(out, maintenanceDynamic(window))
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if out[i].Pinned && out[i].PinPosition != out[j].PinPosition {
			return out[i].PinPosition < out[j].PinPosition
		}
		if out[i].StartAt != out[j].StartAt {
			return out[i].StartAt > out[j].StartAt
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func (s *Store) attachUpdates(dynamics []Dynamic, byID map[int64]int) error {
	if len(byID) == 0 {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT id, dynamic_id, at, label, body FROM dynamic_updates ORDER BY at ASC, id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u DynamicUpdate
		var dynID int64
		if err := rows.Scan(&u.ID, &dynID, &u.At, &u.Label, &u.Body); err != nil {
			return err
		}
		if idx, ok := byID[dynID]; ok {
			dynamics[idx].Updates = append(dynamics[idx].Updates, u)
		}
	}
	return rows.Err()
}

// GetDynamics 返回单条动态（含自动事件的合并结果）。
func (s *Store) GetDynamics(rawID string) (*Dynamic, error) {
	kind, id, err := parseDynamicID(rawID)
	if err != nil {
		return nil, sql.ErrNoRows
	}
	if kind == 'm' {
		window, err := s.GetMaintenance(id)
		if err != nil {
			return nil, err
		}
		d := maintenanceDynamic(*window)
		return &d, nil
	}
	if kind == 'i' {
		incs, err := s.Incidents(1000)
		if err != nil {
			return nil, err
		}
		for _, in := range incs {
			if in.ID != id {
				continue
			}
			state := StateActive
			if in.ResolvedAt != nil {
				state = StateResolve
			}
			d := Dynamic{
				ID: rawID, Kind: KindIncident,
				Title: fmt.Sprintf("%s %s 故障", in.Group, in.ChannelID), Body: in.Reason,
				State: state, StartAt: in.StartedAt, EndAt: in.ResolvedAt,
				Services: []string{in.Group}, Updates: []DynamicUpdate{}, Source: in.Source,
				CreatedAt: in.StartedAt, UpdatedAt: in.StartedAt,
			}
			if in.ResolvedAt != nil {
				d.Updates = append(d.Updates, DynamicUpdate{At: *in.ResolvedAt, Label: "已恢复", Body: "探测恢复正常，故障自动解除。"})
			}
			return &d, nil
		}
		return nil, sql.ErrNoRows
	}

	list, err := s.ListDynamics(1000)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == rawID {
			return &list[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

// migrateDynamics 建立动态相关表；老库的 announcements 一次性迁进来。
func (s *Store) migrateDynamics() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS dynamics (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			kind         TEXT NOT NULL DEFAULT 'notice',
			title        TEXT NOT NULL,
			body         TEXT NOT NULL DEFAULT '',
			state        TEXT NOT NULL DEFAULT 'published',
			start_at     INTEGER NOT NULL,
			end_at       INTEGER,
			pinned       INTEGER NOT NULL DEFAULT 0,
			pin_position INTEGER NOT NULL DEFAULT 0,
			services     TEXT NOT NULL DEFAULT '[]',
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dynamics_start ON dynamics(start_at)`,
		`CREATE TABLE IF NOT EXISTS dynamic_updates (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			dynamic_id INTEGER NOT NULL,
			at         INTEGER NOT NULL,
			label      TEXT NOT NULL DEFAULT '',
			body       TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dynamic_updates_parent ON dynamic_updates(dynamic_id, at)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	var migrated string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'dynamics_migrated'`).Scan(&migrated); err == nil {
		return nil
	} else if err != sql.ErrNoRows {
		return err
	}
	// 旧 announcements 表存在则迁移为 notice 动态。
	var hasOld int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='announcements'`).Scan(&hasOld); err != nil {
		return err
	}
	if hasOld > 0 {
		if _, err := s.db.Exec(
			`INSERT INTO dynamics (kind, title, body, state, start_at, end_at, pinned, pin_position, services, created_at, updated_at)
			 SELECT 'notice', title, body, 'published', ts, NULL, pinned, 0, '[]', ts, ts FROM announcements`); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('dynamics_migrated', '1')`)
	return err
}
