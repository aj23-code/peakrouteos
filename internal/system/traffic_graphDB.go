package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	_ "modernc.org/sqlite" // Pure Go SQLite driver (CGO_ENABLED=0 friendly)
)

const (
	// Samples are averaged per minute in RAM and written in a single transaction at this
	// interval. Fewer, smaller writes mean far less flash wear; a power loss costs at most
	// this much history.
	defaultFlushInterval = 60 * time.Second

	retentionDays          = 30
	retentionCheckInterval = 24 * time.Hour
)

type MetricSample struct {
	Iface     string
	Timestamp int64 // Unix epoch seconds
	RxBytes   int64
	TxBytes   int64
}

type TrafficHistoryResponse struct {
	Rx         []int64 `json:"rx"`
	Tx         []int64 `json:"tx"`
	Timestamps []int64 `json:"timestamps"` // Unix epoch milliseconds for JS Date
}

type TrafficDB struct {
	db            *sql.DB
	sampleCh      chan MetricSample
	flushInterval time.Duration
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

// NewTrafficDB initializes the SQLite database and starts background workers.
func NewTrafficDB(dbPath string) (*TrafficDB, error) {
	return newTrafficDB(dbPath, defaultFlushInterval)
}

func newTrafficDB(dbPath string, flushInterval time.Duration) (*TrafficDB, error) {
	// Open SQLite with Write-Ahead Logging (WAL) for concurrent reads/writes. auto_vacuum=2
	// (incremental) lets the daily purge actually return freed pages to the filesystem.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(2)")
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Schema setup
	schema := `
	CREATE TABLE IF NOT EXISTS interface_metrics (
		iface TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		rx_bytes INTEGER NOT NULL,
		tx_bytes INTEGER NOT NULL,
		PRIMARY KEY (iface, timestamp)
	);
	CREATE INDEX IF NOT EXISTS idx_iface_ts ON interface_metrics (iface, timestamp);
	`

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run database migration: %w", err)
	}

	if err := ensureIncrementalVacuum(db); err != nil {
		db.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	tdb := &TrafficDB{
		db:            db,
		sampleCh:      make(chan MetricSample, 2000), // Non-blocking buffer
		flushInterval: flushInterval,
		cancel:        cancel,
	}

	// Drop expired history now, not only after a day of uptime, so devices that reboot often still prune
	tdb.purgeOld()

	// Launch background workers
	tdb.wg.Add(2)
	go tdb.startFlusher(ctx)
	go tdb.startRetentionCleaner(ctx)

	return tdb, nil
}

// ensureIncrementalVacuum converts a database created before auto_vacuum was enabled.
// The setting only applies to new files, so existing ones need a one-time VACUUM.
func ensureIncrementalVacuum(db *sql.DB) error {
	var mode int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return fmt.Errorf("failed to read auto_vacuum mode: %w", err)
	}
	if mode == 2 {
		return nil
	}

	log.Printf("[TrafficDB] Converting existing database to incremental auto_vacuum")
	if _, err := db.Exec("VACUUM"); err != nil {
		return fmt.Errorf("failed to enable incremental auto_vacuum: %w", err)
	}
	return nil
}

// RecordSample non-blockingly queues a 1-second interface sample for disk batching.
func (tdb *TrafficDB) RecordSample(iface string, rxBytes, txBytes int64) {
	select {
	case tdb.sampleCh <- MetricSample{
		Iface:     iface,
		Timestamp: time.Now().Unix(),
		RxBytes:   rxBytes,
		TxBytes:   txBytes,
	}:
	default:
		log.Printf("[TrafficDB] Warning: sample channel buffer full, dropping tick for interface %s", iface)
	}
}

// GetHistory queries and downsamples database records for 1h, 24h, and 30d views.
func (tdb *TrafficDB) GetHistory(iface string, timeRange string) (*TrafficHistoryResponse, error) {
	now := time.Now().Unix()
	var startTime int64
	var bucketSeconds int64

	switch timeRange {
	case "24h":
		startTime = now - (24 * 3600)
		bucketSeconds = 900 // 15-minute aggregate buckets
	case "30d":
		startTime = now - (30 * 86400)
		bucketSeconds = 3600 // 1-hour aggregate buckets
	default: // "1h" or fallback
		startTime = now - 3600
		bucketSeconds = 60 // 1-minute aggregate buckets
	}

	// Dynamic SQL query downsamples telemetry using bucket integer division
	query := `
	SELECT
	(timestamp / ?) * ? AS bucket,
	CAST(AVG(rx_bytes) AS INTEGER) as avg_rx,
	CAST(AVG(tx_bytes) AS INTEGER) as avg_tx
	FROM interface_metrics
	WHERE iface = ? AND timestamp >= ?
	GROUP BY bucket
	ORDER BY bucket ASC
	`

	rows, err := tdb.db.Query(query, bucketSeconds, bucketSeconds, iface, startTime)
	if err != nil {
		return nil, fmt.Errorf("query error: %w", err)
	}
	defer rows.Close()

	resp := &TrafficHistoryResponse{
		Rx:         make([]int64, 0),
		Tx:         make([]int64, 0),
		Timestamps: make([]int64, 0),
	}

	for rows.Next() {
		var ts, rx, tx int64
		if err := rows.Scan(&ts, &rx, &tx); err != nil {
			continue
		}
		resp.Timestamps = append(resp.Timestamps, ts*1000) // Convert Unix epoch to milliseconds for JS Date
		resp.Rx = append(resp.Rx, rx)
		resp.Tx = append(resp.Tx, tx)
	}

	return resp, nil
}

// startFlusher averages incoming samples into per-minute rows and writes the ones that changed
// to SQLite in a single transaction every flushInterval.
func (tdb *TrafficDB) startFlusher(ctx context.Context) {
	defer tdb.wg.Done()
	ticker := time.NewTicker(tdb.flushInterval)
	defer ticker.Stop()

	agg := newMinuteAggregator()

	for {
		select {
		case <-ctx.Done():
			// Pick up anything still queued in the channel, then flush everything on shutdown
			for {
				select {
				case sample := <-tdb.sampleCh:
					agg.add(sample)
				default:
					tdb.flush(agg.changed())
					return
				}
			}

		case sample := <-tdb.sampleCh:
			agg.add(sample)

		case <-ticker.C:
			tdb.flush(agg.changed())
			agg.expire(time.Now())
		}
	}
}

func (tdb *TrafficDB) flush(samples []MetricSample) {
	if len(samples) == 0 {
		return
	}

	tx, err := tdb.db.Begin()
	if err != nil {
		log.Printf("[TrafficDB] Error starting transaction: %v", err)
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
	INSERT OR REPLACE INTO interface_metrics (iface, timestamp, rx_bytes, tx_bytes)
	VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		log.Printf("[TrafficDB] Error preparing statement: %v", err)
		return
	}
	defer stmt.Close()

	for _, s := range samples {
		if _, err := stmt.Exec(s.Iface, s.Timestamp, s.RxBytes, s.TxBytes); err != nil {
			log.Printf("[TrafficDB] Exec error for %s: %v", s.Iface, err)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[TrafficDB] Commit error: %v", err)
	}
}

// purgeOld deletes metrics past the retention window and returns the freed pages to the filesystem.
func (tdb *TrafficDB) purgeOld() {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	if _, err := tdb.db.Exec("DELETE FROM interface_metrics WHERE timestamp < ?", cutoff); err != nil {
		log.Printf("[TrafficDB] Retention purge error: %v", err)
		return
	}
	_, _ = tdb.db.Exec("PRAGMA incremental_vacuum")
}

// startRetentionCleaner purges expired metrics once a day (the first purge runs at startup).
func (tdb *TrafficDB) startRetentionCleaner(ctx context.Context) {
	defer tdb.wg.Done()
	ticker := time.NewTicker(retentionCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tdb.purgeOld()
		}
	}
}

// HandleTrafficHistory handles HTTP GET requests for historical bandwidth metrics
func (tdb *TrafficDB) HandleTrafficHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	iface := r.URL.Query().Get("iface")
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "1h"
	}

	history, err := tdb.GetHistory(iface, timeRange)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch traffic history: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	json.NewEncoder(w).Encode(history)
}

// Close gracefully flushes pending metrics and closes the database connection.
func (tdb *TrafficDB) Close() error {
	tdb.cancel()
	tdb.wg.Wait()
	return tdb.db.Close()
}
