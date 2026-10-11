// Package fwlog keeps the firewall log: the packets that rules with "log" switched on matched, read from the kernel log
// and saved, parsed, in a small database of their own. The system journal is shared with everything else and is size
// limited, so a busy rule must not be able to push the rest of the system's messages out, and the journal rotating must
// not take the firewall log with it. docs/firewall.md (section 7) describes what is kept and for how long.
package fwlog

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"peakroute/internal/network"

	_ "modernc.org/sqlite" // pure Go SQLite driver, as for the traffic history
)

const (
	// DefaultKeepDays and DefaultKeepEntries are what the log holds: entries older than the days, or the oldest of more
	// than the entries, are dropped. An entry is a row: repeats are folded into one (see Window).
	DefaultKeepDays    = 7
	DefaultKeepEntries = 100000

	// Window is how long the same kind of packet (same rule, interfaces, addresses, protocol and destination port) keeps
	// being counted in one entry. A port scan then costs one row and says "seen 342 times", not 342 rows.
	Window = time.Minute

	pruneEvery = time.Minute
)

// Event is one entry of the log: a kind of packet, how many times it was seen, and when.
type Event struct {
	ID      int64
	Time    int64 // Unix milliseconds: the first time
	Last    int64 // the last time (equal to Time for a single packet)
	Rule    string
	In, Out string
	Src     string
	SrcPort int // of the first packet
	Dst     string
	DstPort int
	Proto   string
	Count   int
}

// Stats says how much the log holds.
type Stats struct {
	Entries     int
	Oldest      int64 // Unix milliseconds of the oldest entry; 0 when empty
	KeepDays    int   // what the log holds: entries older than this are dropped...
	KeepEntries int   // ...and the oldest of more than this many
}

// Query selects entries. Everything is optional.
type Query struct {
	Rules    []string // only these rules; honoured only when RulesSet (an empty set then matches nothing)
	RulesSet bool
	Since    int64  // Unix milliseconds: only entries seen at or after it
	Search   string // part of an address, interface, protocol or a port number
	Before   int64  // only entries older than this id (the cursor for the next page)
	Limit    int    // at most this many, newest first; 0: no limit
}

// key says what makes two packets "the same kind".
type key struct {
	rule, in, out, src, dst, proto string
	dport                          int
}

type openEntry struct {
	id   int64
	last int64
}

// Store is the firewall log database.
type Store struct {
	db          *sql.DB
	KeepEntries int
	KeepAge     time.Duration
	now         func() time.Time

	mu        sync.Mutex
	recent    map[key]openEntry
	lastPrune time.Time
}

// Open opens (creating if needed) the log in the given file.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(2)")
	if err != nil {
		return nil, fmt.Errorf("open firewall log: %w", err)
	}
	db.SetMaxOpenConns(1) // one writer; the queries are short
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time INTEGER NOT NULL,
			last INTEGER NOT NULL,
			rule TEXT NOT NULL,
			in_if TEXT NOT NULL,
			out_if TEXT NOT NULL,
			src TEXT NOT NULL,
			sport INTEGER NOT NULL,
			dst TEXT NOT NULL,
			dport INTEGER NOT NULL,
			proto TEXT NOT NULL,
			count INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS events_last ON events(last)`,
		`CREATE INDEX IF NOT EXISTS events_rule ON events(rule)`,
		`CREATE TABLE IF NOT EXISTS meta (name TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("prepare firewall log: %w", err)
		}
	}
	s := &Store{db: db, KeepEntries: DefaultKeepEntries, KeepAge: DefaultKeepDays * 24 * time.Hour, now: time.Now, recent: map[key]openEntry{}}
	if err := s.prune(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Cursor is where reading the journal stopped (empty: never read).
func (s *Store) Cursor() string {
	var v string
	s.db.QueryRow(`SELECT value FROM meta WHERE name = 'cursor'`).Scan(&v)
	return v
}

// ResetCursor forgets where the journal was read to, so that reading starts again from the start of what the journal
// keeps (a cursor journalctl refuses would otherwise stop the log for good).
func (s *Store) ResetCursor() error {
	_, err := s.db.Exec(`DELETE FROM meta WHERE name = 'cursor'`)
	return err
}

func keyOf(e network.FirewallLogEntry) key {
	return key{e.Rule, e.In, e.Out, e.Src, e.Dst, e.Proto, e.DstPort}
}

// Add saves a batch of packets in one transaction, folding repeats into the entry they repeat, and remembers the journal
// cursor of the last of them (when not empty). Old entries are dropped from time to time.
func (s *Store) Add(batch []network.FirewallLogEntry, cursor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	window := Window.Milliseconds()
	pending := map[key]openEntry{} // applied only when the transaction commits
	for _, e := range batch {
		k := keyOf(e)
		cur, ok := pending[k]
		if !ok {
			cur, ok = s.recent[k]
		}
		if ok && e.Time >= cur.last && e.Time-cur.last <= window {
			if _, err := tx.Exec(`UPDATE events SET count = count + 1, last = ? WHERE id = ?`, e.Time, cur.id); err != nil {
				return err
			}
			pending[k] = openEntry{cur.id, e.Time}
			continue
		}
		res, err := tx.Exec(`INSERT INTO events (time, last, rule, in_if, out_if, src, sport, dst, dport, proto, count) VALUES (?,?,?,?,?,?,?,?,?,?,1)`,
			e.Time, e.Time, e.Rule, e.In, e.Out, e.Src, e.SrcPort, e.Dst, e.DstPort, e.Proto)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		pending[k] = openEntry{id, e.Time}
	}
	if cursor != "" {
		if _, err := tx.Exec(`INSERT INTO meta (name, value) VALUES ('cursor', ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value`, cursor); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for k, v := range pending {
		s.recent[k] = v
	}
	// forget what can no longer be repeated
	if len(s.recent) > 0 {
		var newest int64
		for _, v := range s.recent {
			if v.last > newest {
				newest = v.last
			}
		}
		for k, v := range s.recent {
			if newest-v.last > window {
				delete(s.recent, k)
			}
		}
	}
	if s.now().Sub(s.lastPrune) >= pruneEvery {
		return s.pruneLocked()
	}
	return nil
}

func (s *Store) prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked()
}

// pruneLocked drops what is older than KeepAge and the oldest entries beyond KeepEntries.
func (s *Store) pruneLocked() error {
	s.lastPrune = s.now()
	cutoff := s.now().Add(-s.KeepAge).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM events WHERE last < ?`, cutoff); err != nil {
		return err
	}
	if s.KeepEntries > 0 {
		if _, err := s.db.Exec(`DELETE FROM events WHERE id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET ?)`, s.KeepEntries); err != nil {
			return err
		}
	}
	// give the freed pages back, so that the file stays about as big as the log
	s.db.Exec(`PRAGMA incremental_vacuum`)
	return nil
}

// Stats says how many entries the log holds and since when.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	var oldest sql.NullInt64
	err := s.db.QueryRow(`SELECT COUNT(*), MIN(time) FROM events`).Scan(&st.Entries, &oldest)
	st.Oldest = oldest.Int64
	st.KeepDays = int(s.KeepAge / (24 * time.Hour))
	st.KeepEntries = s.KeepEntries
	return st, err
}

// likeEscape makes text safe to use inside LIKE '%...%' (with ESCAPE '\').
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Query returns the entries that match, newest first.
func (s *Store) Query(q Query) ([]Event, error) {
	var where []string
	var args []any
	if q.RulesSet {
		if len(q.Rules) == 0 {
			return []Event{}, nil
		}
		where = append(where, "rule IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Rules)), ",")+")")
		for _, r := range q.Rules {
			args = append(args, r)
		}
	}
	if q.Since > 0 {
		where = append(where, "last >= ?")
		args = append(args, q.Since)
	}
	if q.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, q.Before)
	}
	if t := strings.TrimSpace(q.Search); t != "" {
		like := "%" + likeEscape(t) + "%"
		cond := `(src LIKE ? ESCAPE '\' OR dst LIKE ? ESCAPE '\' OR in_if LIKE ? ESCAPE '\' OR out_if LIKE ? ESCAPE '\' OR proto LIKE ? ESCAPE '\'`
		args = append(args, like, like, like, like, like)
		if n, err := strconv.Atoi(t); err == nil {
			cond += ` OR dport = ? OR sport = ?`
			args = append(args, n, n)
		}
		where = append(where, cond+")")
	}
	sqlText := `SELECT id, time, last, rule, in_if, out_if, src, sport, dst, dport, proto, count FROM events`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY id DESC"
	if q.Limit > 0 {
		sqlText += " LIMIT " + strconv.Itoa(q.Limit)
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Time, &e.Last, &e.Rule, &e.In, &e.Out, &e.Src, &e.SrcPort, &e.Dst, &e.DstPort, &e.Proto, &e.Count); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
