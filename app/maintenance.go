package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrMaintenanceInput = errors.New("invalid maintenance window")

type MaintenanceInput struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	StartAt     int64    `json:"start_at"`
	EndAt       int64    `json:"end_at"`
	AllChannels bool     `json:"all_channels"`
	ChannelIDs  []string `json:"channel_ids"`
}

type MaintenanceWindow struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	StartAt     int64    `json:"start_at"`
	EndAt       int64    `json:"end_at"`
	AllChannels bool     `json:"all_channels"`
	ChannelIDs  []string `json:"channel_ids"`
	CancelledAt *int64   `json:"cancelled_at"`
	CreatedAt   int64    `json:"created_at"`
	State       string   `json:"state"`
}

func (m MaintenanceWindow) stateAt(now int64) string {
	if m.CancelledAt != nil && *m.CancelledAt <= now {
		return "cancelled"
	}
	if now < m.StartAt {
		return "scheduled"
	}
	if now < m.EndAt {
		return "active"
	}
	return "completed"
}

func (m MaintenanceWindow) effectiveEnd() int64 {
	if m.CancelledAt != nil && *m.CancelledAt < m.EndAt {
		return *m.CancelledAt
	}
	return m.EndAt
}

func (s *Store) migrateMaintenance() error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS maintenance_windows (
		 id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '',
		 start_at INTEGER NOT NULL, end_at INTEGER NOT NULL, all_channels INTEGER NOT NULL DEFAULT 0,
		 cancelled_at INTEGER, created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_times ON maintenance_windows(start_at, end_at)`,
		`CREATE TABLE IF NOT EXISTS maintenance_channels (
		 maintenance_id INTEGER NOT NULL, channel_id TEXT NOT NULL, PRIMARY KEY(maintenance_id, channel_id))`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_channel ON maintenance_channels(channel_id, maintenance_id)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return s.ensureColumn("probes", "maintenance_id", "INTEGER")
}

func (s *Store) CreateMaintenance(in MaintenanceInput) (*MaintenanceWindow, error) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	now := s.now().Unix()
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > 300 || len(in.Body) > 16000 {
		return nil, fmt.Errorf("%w: title is required (max 300 bytes); body max 16000 bytes", ErrMaintenanceInput)
	}
	if in.StartAt == 0 {
		in.StartAt = now
	}
	if in.StartAt < now-60 || in.StartAt > now+365*daySeconds {
		return nil, fmt.Errorf("%w: start must be now or within the next 365 days", ErrMaintenanceInput)
	}
	if in.StartAt < now {
		in.StartAt = now
	}
	if in.EndAt <= in.StartAt || in.EndAt-in.StartAt > 30*daySeconds {
		return nil, fmt.Errorf("%w: end must follow start; duration max 30 days", ErrMaintenanceInput)
	}
	if in.AllChannels && len(in.ChannelIDs) != 0 {
		return nil, fmt.Errorf("%w: all_channels and channel_ids cannot be combined", ErrMaintenanceInput)
	}
	if !in.AllChannels && (len(in.ChannelIDs) == 0 || len(in.ChannelIDs) > 200) {
		return nil, fmt.Errorf("%w: select 1..200 channels or all_channels", ErrMaintenanceInput)
	}
	seen := map[string]bool{}
	for _, id := range in.ChannelIDs {
		if !channelIDPattern.MatchString(id) || seen[id] {
			return nil, fmt.Errorf("%w: invalid or duplicate channel ID", ErrMaintenanceInput)
		}
		seen[id] = true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, id := range in.ChannelIDs {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = ?`, id).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			return nil, fmt.Errorf("%w: channel %s does not exist", ErrMaintenanceInput, id)
		}
	}
	res, err := tx.Exec(`INSERT INTO maintenance_windows(title,body,start_at,end_at,all_channels,created_at) VALUES(?,?,?,?,?,?)`, in.Title, in.Body, in.StartAt, in.EndAt, boolInt(in.AllChannels), now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	for _, cid := range in.ChannelIDs {
		if _, err := tx.Exec(`INSERT INTO maintenance_channels(maintenance_id,channel_id) VALUES(?,?)`, id, cid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetMaintenance(id)
}

const maintenanceColumns = `m.id,m.title,m.body,m.start_at,m.end_at,m.all_channels,m.cancelled_at,m.created_at,
 COALESCE((SELECT json_group_array(channel_id) FROM maintenance_channels WHERE maintenance_id=m.id),'[]')`

func scanMaintenance(row scanner, now int64) (MaintenanceWindow, error) {
	var m MaintenanceWindow
	var cancelled sql.NullInt64
	var all int
	var channels string
	err := row.Scan(&m.ID, &m.Title, &m.Body, &m.StartAt, &m.EndAt, &all, &cancelled, &m.CreatedAt, &channels)
	if err != nil {
		return m, err
	}
	m.AllChannels = all == 1
	m.CancelledAt = nullInt(cancelled)
	m.ChannelIDs = decodeServices(channels)
	m.State = m.stateAt(now)
	return m, nil
}

func (s *Store) GetMaintenance(id int64) (*MaintenanceWindow, error) {
	m, err := scanMaintenance(s.db.QueryRow(`SELECT `+maintenanceColumns+` FROM maintenance_windows m WHERE m.id=?`, id), s.now().Unix())
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) ListMaintenance(upcomingOnly bool, limit int) ([]MaintenanceWindow, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	now := s.now().Unix()
	query := `SELECT ` + maintenanceColumns + ` FROM maintenance_windows m`
	args := []any{}
	if upcomingOnly {
		query += ` WHERE m.cancelled_at IS NULL AND m.end_at>?`
		args = append(args, now)
	}
	query += ` ORDER BY CASE WHEN m.cancelled_at IS NULL AND m.start_at<=? AND m.end_at>? THEN 0 WHEN m.cancelled_at IS NULL AND m.start_at>? THEN 1 ELSE 2 END,
 CASE WHEN m.cancelled_at IS NULL AND m.end_at>? THEN m.start_at ELSE -m.start_at END, m.id DESC LIMIT ?`
	args = append(args, now, now, now, now, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MaintenanceWindow{}
	for rows.Next() {
		m, err := scanMaintenance(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) CancelMaintenance(id int64) error {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	now := s.now().Unix()
	res, err := s.db.Exec(`UPDATE maintenance_windows SET cancelled_at=? WHERE id=? AND cancelled_at IS NULL AND end_at>?`, now, id, now)
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

// Queries use [start_at, effective_end) so the exact end instant resumes normal monitoring.
func (s *Store) MaintenanceForProbe(channelID string, startedAt, finishedAt int64) (*MaintenanceWindow, error) {
	m, err := scanMaintenance(s.db.QueryRow(`SELECT `+maintenanceColumns+` FROM maintenance_windows m
 WHERE m.start_at<=? AND m.end_at>? AND (m.cancelled_at IS NULL OR (m.cancelled_at>? AND m.cancelled_at>m.start_at))
 AND (m.all_channels=1 OR EXISTS(SELECT 1 FROM maintenance_channels c WHERE c.maintenance_id=m.id AND c.channel_id=?))
 ORDER BY COALESCE(m.cancelled_at,m.end_at) DESC,m.id DESC LIMIT 1`, finishedAt, startedAt, startedAt, channelID), finishedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) LatestMaintenanceEnd(channelID string, now int64) (int64, error) {
	var end sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(COALESCE(m.cancelled_at,m.end_at)) FROM maintenance_windows m
 WHERE COALESCE(m.cancelled_at,m.end_at)<=? AND COALESCE(m.cancelled_at,m.end_at)>m.start_at
 AND (m.all_channels=1 OR EXISTS(SELECT 1 FROM maintenance_channels c WHERE c.maintenance_id=m.id AND c.channel_id=?))`, now, channelID).Scan(&end)
	return end.Int64, err
}

func (s *Store) ActiveMaintenanceByChannel(channels []Channel) (map[string]*MaintenanceWindow, error) {
	out := map[string]*MaintenanceWindow{}
	now := s.now().Unix()
	for _, ch := range channels {
		m, err := s.MaintenanceForProbe(ch.ID, now, now)
		if err != nil {
			return nil, err
		}
		if m != nil {
			out[ch.ID] = m
		}
	}
	return out, nil
}
