package system

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"encoding/json"

	_ "modernc.org/sqlite" // Pure Go SQLite driver (CGO_ENABLED=0 friendly)
"log"
"sync"
"time"
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
	db       *sql.DB
	sampleCh chan MetricSample
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewTrafficDB initializes the SQLite database and starts background workers.
func NewTrafficDB(dbPath string) (*TrafficDB, error) {
	// Open SQLite with Write-Ahead Logging (WAL) for high-performance concurrent reads/writes
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
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

		ctx, cancel := context.WithCancel(context.Background())

		tdb := &TrafficDB{
			db:       db,
			sampleCh: make(chan MetricSample, 2000), // Non-blocking buffer
			cancel:   cancel,
		}

		// Launch background workers
		tdb.wg.Add(2)
		go tdb.startFlusher(ctx)
		go tdb.startRetentionCleaner(ctx)

		return tdb, nil
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

// startFlusher drains the RAM buffer to SQLite in single transactions every 10 seconds.
func (tdb *TrafficDB) startFlusher(ctx context.Context) {
	defer tdb.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var buffer []MetricSample

	for {
		select {
			case <-ctx.Done():
				// Flush remaining items on shutdown
				tdb.flush(buffer)
				return

			case sample := <-tdb.sampleCh:
				buffer = append(buffer, sample)

			case <-ticker.C:
				if len(buffer) > 0 {
					tdb.flush(buffer)
					buffer = nil // Reset buffer slice
				}
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

// startRetentionCleaner runs once daily to delete metrics older than 30 days.
func (tdb *TrafficDB) startRetentionCleaner(ctx context.Context) {
	defer tdb.wg.Done()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().AddDate(0, 0, -30).Unix()
				if _, err := tdb.db.Exec("DELETE FROM interface_metrics WHERE timestamp < ?", cutoff); err != nil {
					log.Printf("[TrafficDB] Retention purge error: %v", err)
				} else {
					_, _ = tdb.db.Exec("PRAGMA incremental_vacuum;")
				}
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
