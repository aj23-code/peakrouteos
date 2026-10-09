package wanhealth

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, as for the traffic history
)

// Probes are folded into one row per target per minute in RAM and written in a single
// transaction once a minute (the same approach, and for the same flash wear reasons, as the
// traffic history). A power cut costs at most the last minute.

const (
	minuteSeconds  = 60
	retentionDays  = 30
	ringCapacity   = 120 // raw probes kept for the 5 minute view (10 minutes at 5 s)
	keepMinutesRAM = 3   // minutes held in RAM after they end, so late probes still merge
)

// agg sums the probes of one period. RTTs are microseconds.
type agg struct {
	sent, lost int64
	rttSum     int64
	rttMin     int64 // only meaningful when there are replies
	rttMax     int64
	dirty      bool
}

func (a *agg) replies() int64 { return a.sent - a.lost }

func (a *agg) addProbe(rttUS int64, ok bool) {
	a.sent++
	a.dirty = true
	if !ok {
		a.lost++
		return
	}
	a.rttSum += rttUS
	if a.replies() == 1 || rttUS < a.rttMin {
		a.rttMin = rttUS
	}
	if rttUS > a.rttMax {
		a.rttMax = rttUS
	}
}

// merge adds b's probes to a.
func (a *agg) merge(b agg) {
	if b.replies() > 0 {
		if a.replies() == 0 || b.rttMin < a.rttMin {
			a.rttMin = b.rttMin
		}
		if b.rttMax > a.rttMax {
			a.rttMax = b.rttMax
		}
	}
	a.sent += b.sent
	a.lost += b.lost
	a.rttSum += b.rttSum
}

type minuteKey struct {
	target string
	minute int64
}

// Probe is one raw measurement.
type Probe struct {
	At     time.Time
	Target string
	RTT    time.Duration
	OK     bool
}

type Store struct {
	db    *sql.DB // nil: memory only (tests)
	mu    sync.Mutex
	mins  map[minuteKey]*agg
	ring  []Probe
	flush time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open opens (creating if needed) the history database and starts the once-a-minute writer.
// An empty path keeps everything in memory.
func Open(path string) (*Store, error) { return open(path, time.Minute) }

func open(path string, flush time.Duration) (*Store, error) {
	s := &Store{mins: map[minuteKey]*agg{}, flush: flush}
	if path != "" {
		db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(2)")
		if err != nil {
			return nil, fmt.Errorf("open wan health database: %w", err)
		}
		_, err = db.Exec(`CREATE TABLE IF NOT EXISTS wan_health (
			target TEXT NOT NULL,
			minute INTEGER NOT NULL,
			sent INTEGER NOT NULL,
			lost INTEGER NOT NULL,
			rtt_sum_us INTEGER NOT NULL,
			rtt_min_us INTEGER NOT NULL,
			rtt_max_us INTEGER NOT NULL,
			PRIMARY KEY (target, minute)
		)`)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("create wan health table: %w", err)
		}
		s.db = db
		s.purge(time.Now())
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.flusher(ctx)
	return s, nil
}

// Record adds one probe.
func (s *Store) Record(p Probe) {
	s.mu.Lock()
	defer s.mu.Unlock()

	us := p.RTT.Microseconds()
	key := minuteKey{p.Target, p.At.Unix() / minuteSeconds * minuteSeconds}
	a := s.mins[key]
	if a == nil {
		a = &agg{}
		s.mins[key] = a
	}
	a.addProbe(us, p.OK)

	s.ring = append(s.ring, p)
	if len(s.ring) > ringCapacity {
		s.ring = append(s.ring[:0], s.ring[len(s.ring)-ringCapacity:]...)
	}
}

func (s *Store) flusher(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.flush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.writeDirty()
			return
		case now := <-t.C:
			s.writeDirty()
			s.expire(now)
			if now.Minute() == 0 && now.Hour() == 3 { // once a day is enough; at the quietest hour
				s.purge(now)
			}
		}
	}
}

func (s *Store) writeDirty() {
	if s.db == nil {
		return
	}
	type row struct {
		k minuteKey
		a agg
	}
	var rows []row
	s.mu.Lock()
	for k, a := range s.mins {
		if a.dirty {
			rows = append(rows, row{k, *a})
			a.dirty = false
		}
	}
	s.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	// A failed write leaves the minutes marked as changed so the next flush tries again
	redo := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, r := range rows {
			if a := s.mins[r.k]; a != nil {
				a.dirty = true
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("[wanhealth] begin: %v", err)
		redo()
		return
	}
	defer tx.Rollback()
	for _, r := range rows {
		_, err := tx.Exec(`INSERT OR REPLACE INTO wan_health (target, minute, sent, lost, rtt_sum_us, rtt_min_us, rtt_max_us) VALUES (?,?,?,?,?,?,?)`,
			r.k.target, r.k.minute, r.a.sent, r.a.lost, r.a.rttSum, r.a.rttMin, r.a.rttMax)
		if err != nil {
			log.Printf("[wanhealth] write: %v", err)
			redo()
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[wanhealth] commit: %v", err)
		redo()
	}
}

// expire forgets minutes that ended a while ago and have been written.
func (s *Store) expire(now time.Time) {
	cutoff := now.Unix()/minuteSeconds*minuteSeconds - keepMinutesRAM*minuteSeconds
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, a := range s.mins {
		if k.minute < cutoff && !a.dirty {
			delete(s.mins, k)
		}
	}
}

func (s *Store) purge(now time.Time) {
	if s.db == nil {
		return
	}
	cutoff := now.AddDate(0, 0, -retentionDays).Unix()
	if _, err := s.db.Exec(`DELETE FROM wan_health WHERE minute < ?`, cutoff); err != nil {
		log.Printf("[wanhealth] purge: %v", err)
		return
	}
	_, _ = s.db.Exec(`PRAGMA incremental_vacuum`)
}

// Close writes what is pending and closes the database.
func (s *Store) Close() error {
	s.cancel()
	s.wg.Wait()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ---- reading -------------------------------------------------------------------------

// Range is one of the time windows of the chart.
type Range struct {
	Name   string
	Window time.Duration
	Bucket time.Duration // 0: raw probes
}

var Ranges = map[string]Range{
	"5m":  {"5m", 5 * time.Minute, 0},
	"1h":  {"1h", time.Hour, time.Minute},
	"24h": {"24h", 24 * time.Hour, 15 * time.Minute},
	"30d": {"30d", 30 * 24 * time.Hour, time.Hour},
}

// Point is one chart point. The RTTs are milliseconds, nil when every probe in it was lost.
type Point struct {
	At      int64 // Unix milliseconds
	Sent    int64
	Lost    int64
	AvgMS   *float64
	MinMS   *float64
	MaxMS   *float64
	LossPct float64
}

// Summary covers the whole window.
type Summary struct {
	Sent    int64    `json:"sent"`
	Lost    int64    `json:"lost"`
	LossPct float64  `json:"loss_pct"`
	AvgMS   *float64 `json:"avg_ms"`
	MinMS   *float64 `json:"min_ms"`
	MaxMS   *float64 `json:"max_ms"`
}

func ms(us int64) *float64 { v := float64(us) / 1000; return &v }

func pointOf(at time.Time, a agg) Point {
	p := Point{At: at.UnixMilli(), Sent: a.sent, Lost: a.lost}
	if a.sent > 0 {
		p.LossPct = float64(a.lost) * 100 / float64(a.sent)
	}
	if r := a.replies(); r > 0 {
		p.AvgMS, p.MinMS, p.MaxMS = ms(a.rttSum/r), ms(a.rttMin), ms(a.rttMax)
	}
	return p
}

// History returns the chart points of a range, oldest first, and their summary.
func (s *Store) History(r Range, now time.Time) ([]Point, Summary, error) {
	start := now.Add(-r.Window)

	if r.Bucket == 0 {
		return s.rawHistory(start)
	}

	bucket := int64(r.Bucket / time.Second)
	buckets := map[int64]*agg{}
	add := func(b int64, a agg) {
		if buckets[b] == nil {
			buckets[b] = &agg{}
		}
		buckets[b].merge(a)
	}

	// RAM holds the newest minutes (exactly, including the ones not written yet); the database the
	// older ones. Reading RAM first and asking the database only for what is older means no minute
	// is counted twice, whether or not it has been written yet.
	s.mu.Lock()
	oldestRAM := now.Unix()/minuteSeconds*minuteSeconds + minuteSeconds
	for k := range s.mins {
		if k.minute < oldestRAM {
			oldestRAM = k.minute
		}
	}
	for k, a := range s.mins {
		if k.minute >= start.Unix() {
			add(k.minute/bucket*bucket, *a)
		}
	}
	s.mu.Unlock()

	if s.db != nil {
		rows, err := s.db.Query(`SELECT minute/?*?, SUM(sent), SUM(lost), SUM(rtt_sum_us),
			MIN(CASE WHEN sent > lost THEN rtt_min_us END), MAX(CASE WHEN sent > lost THEN rtt_max_us END)
			FROM wan_health WHERE minute >= ? AND minute < ? GROUP BY 1`, bucket, bucket, start.Unix(), oldestRAM)
		if err != nil {
			return nil, Summary{}, fmt.Errorf("query wan health: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b int64
			var a agg
			var mn, mx sql.NullInt64
			if err := rows.Scan(&b, &a.sent, &a.lost, &a.rttSum, &mn, &mx); err != nil {
				return nil, Summary{}, err
			}
			a.rttMin, a.rttMax = mn.Int64, mx.Int64
			add(b, a)
		}
		if err := rows.Err(); err != nil {
			return nil, Summary{}, err
		}
	}

	keys := make([]int64, 0, len(buckets))
	for b := range buckets {
		keys = append(keys, b)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var points []Point
	var total agg
	for _, b := range keys {
		a := *buckets[b]
		points = append(points, pointOf(time.Unix(b, 0), a))
		total.merge(a)
	}
	return points, summaryOf(total), nil
}

func (s *Store) rawHistory(start time.Time) ([]Point, Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var points []Point
	var total agg
	for _, p := range s.ring {
		if p.At.Before(start) {
			continue
		}
		var a agg
		a.addProbe(p.RTT.Microseconds(), p.OK)
		points = append(points, pointOf(p.At, a))
		total.merge(a)
	}
	return points, summaryOf(total), nil
}

func summaryOf(a agg) Summary {
	s := Summary{Sent: a.sent, Lost: a.lost}
	if a.sent > 0 {
		s.LossPct = float64(a.lost) * 100 / float64(a.sent)
	}
	if r := a.replies(); r > 0 {
		s.AvgMS, s.MinMS, s.MaxMS = ms(a.rttSum/r), ms(a.rttMin), ms(a.rttMax)
	}
	return s
}

// Recent returns the newest raw probes, oldest first, for judging whether the link is up.
func (s *Store) Recent(n int) []Probe {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.ring) {
		n = len(s.ring)
	}
	return append([]Probe(nil), s.ring[len(s.ring)-n:]...)
}
