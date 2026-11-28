// Package devhistory keeps what each device did over time (docs/device-pages.md, phase 1): how much it sent and received, minute by minute,
// and when it came and went. It stores bytes and presence only, never where a device went. Rows are one minute long for a day, five minutes
// long after that, and dropped after the retention (30 days).
package devhistory

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, as for the traffic and health histories
)

// Defaults.
const (
	DefaultRetention = 30 * 24 * time.Hour
	minuteRows       = 25 * time.Hour // how long the one-minute rows are kept before they are folded into five-minute rows
	fiveMinutes      = 300
)

// Event is a change in a device's presence: it came, it went, or it moved (another Wi-Fi network, band or network of the router).
type Event struct {
	MAC        string `json:"mac"`
	At         int64  `json:"at"` // Unix seconds
	Online     bool   `json:"online"`
	Connection string `json:"connection,omitempty"` // wifi, wired
	SSID       string `json:"ssid,omitempty"`
	Band       string `json:"band,omitempty"`
	Network    string `json:"network,omitempty"`
	IP         string `json:"ip,omitempty"`
}

// Point is the traffic of one bucket: the bytes the device sent and received in the time from T on.
type Point struct {
	T    int64  `json:"t"` // Unix seconds, the start of the bucket
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

// Store is the database.
type Store struct {
	db        *sql.DB
	mu        sync.Mutex
	Retention time.Duration
	Now       func() time.Time // time.Now when nil (the tests set it)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return DefaultRetention
}

// Open opens (creating if needed) the database. An empty path keeps everything in memory.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(2)"
	if path == "" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open device history: %w", err)
	}
	if path == "" {
		db.SetMaxOpenConns(1) // every connection to ":memory:" is a database of its own
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS usage_min (mac TEXT NOT NULL, minute INTEGER NOT NULL, up INTEGER NOT NULL, down INTEGER NOT NULL, PRIMARY KEY (mac, minute))`,
		`CREATE TABLE IF NOT EXISTS usage_5m (mac TEXT NOT NULL, bucket INTEGER NOT NULL, up INTEGER NOT NULL, down INTEGER NOT NULL, PRIMARY KEY (mac, bucket))`,
		`CREATE TABLE IF NOT EXISTS presence (mac TEXT NOT NULL, at INTEGER NOT NULL, online INTEGER NOT NULL, connection TEXT NOT NULL DEFAULT '', ssid TEXT NOT NULL DEFAULT '',
			band TEXT NOT NULL DEFAULT '', network TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', PRIMARY KEY (mac, at))`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("create device history tables: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func norm(mac string) string { return strings.ToLower(strings.TrimSpace(mac)) }

// AddUsage adds bytes to the minute that starts at the given time (rounded down to the minute).
func (s *Store) AddUsage(mac string, at time.Time, up, down uint64) error {
	if up == 0 && down == 0 {
		return nil
	}
	minute := at.Unix() / 60 * 60
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO usage_min (mac, minute, up, down) VALUES (?, ?, ?, ?)
		ON CONFLICT(mac, minute) DO UPDATE SET up = up + excluded.up, down = down + excluded.down`, norm(mac), minute, up, down)
	return err
}

// AddEvent records a change of presence.
func (s *Store) AddEvent(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	online := 0
	if e.Online {
		online = 1
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO presence (mac, at, online, connection, ssid, band, network, ip) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		norm(e.MAC), e.At, online, e.Connection, e.SSID, e.Band, e.Network, e.IP)
	return err
}

// Usage returns the traffic of a device from one time to another in buckets of the given length (a multiple of a minute; over five
// minutes where the range reaches into the folded rows). Buckets with no traffic are left out.
func (s *Store) Usage(mac string, from, to time.Time, bucket time.Duration) ([]Point, error) {
	b := int64(bucket / time.Second)
	if b < 60 {
		b = 60
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT (t / ?) * ? AS b, SUM(up), SUM(down) FROM (
			SELECT minute AS t, up, down FROM usage_min WHERE mac = ? AND minute >= ? AND minute < ?
			UNION ALL
			SELECT bucket AS t, up, down FROM usage_5m WHERE mac = ? AND bucket >= ? AND bucket < ?
		) GROUP BY b ORDER BY b`, b, b, norm(mac), from.Unix(), to.Unix(), norm(mac), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.T, &p.Up, &p.Down); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Events returns a device's changes of presence since a time, the newest first (at most limit; 0: all).
func (s *Store) Events(mac string, since time.Time, limit int) ([]Event, error) {
	q := `SELECT mac, at, online, connection, ssid, band, network, ip FROM presence WHERE mac = ? AND at >= ? ORDER BY at DESC`
	args := []any{norm(mac), since.Unix()}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queryEvents(q, args...)
}

func (s *Store) queryEvents(q string, args ...any) ([]Event, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var online int
		if err := rows.Scan(&e.MAC, &e.At, &online, &e.Connection, &e.SSID, &e.Band, &e.Network, &e.IP); err != nil {
			return nil, err
		}
		e.Online = online == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventBefore returns the newest event of a device before a time (ok is false when there is none).
func (s *Store) EventBefore(mac string, before time.Time) (Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs, err := s.queryEvents(`SELECT mac, at, online, connection, ssid, band, network, ip FROM presence WHERE mac = ? AND at < ? ORDER BY at DESC LIMIT 1`, norm(mac), before.Unix())
	if err != nil || len(evs) == 0 {
		return Event{}, false, err
	}
	return evs[0], true, nil
}

// LastEvents returns the newest event of every device.
func (s *Store) LastEvents() (map[string]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs, err := s.queryEvents(`SELECT p.mac, p.at, p.online, p.connection, p.ssid, p.band, p.network, p.ip FROM presence p
		JOIN (SELECT mac, MAX(at) AS at FROM presence GROUP BY mac) m ON p.mac = m.mac AND p.at = m.at`)
	if err != nil {
		return nil, err
	}
	out := map[string]Event{}
	for _, e := range evs {
		out[e.MAC] = e
	}
	return out, nil
}

// Forget drops everything kept about a device (it was removed from the list).
func (s *Store) Forget(mac string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range []string{"usage_min", "usage_5m", "presence"} {
		if _, err := s.db.Exec(`DELETE FROM `+t+` WHERE mac = ?`, norm(mac)); err != nil {
			return err
		}
	}
	return nil
}

// Compact folds the one-minute rows older than a day into five-minute rows and drops what is older than the retention.
func (s *Store) Compact() error {
	now := s.now()
	fold := now.Add(-minuteRows).Unix()
	drop := now.Add(-s.retention()).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO usage_5m (mac, bucket, up, down) SELECT mac, minute / ? * ?, SUM(up), SUM(down) FROM usage_min WHERE minute < ? GROUP BY mac, minute / ? * ?
			ON CONFLICT(mac, bucket) DO UPDATE SET up = up + excluded.up, down = down + excluded.down`, []any{fiveMinutes, fiveMinutes, fold, fiveMinutes, fiveMinutes}},
		{`DELETE FROM usage_min WHERE minute < ?`, []any{fold}},
		{`DELETE FROM usage_5m WHERE bucket < ?`, []any{drop}},
		{`DELETE FROM presence WHERE at < ?`, []any{drop}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}
