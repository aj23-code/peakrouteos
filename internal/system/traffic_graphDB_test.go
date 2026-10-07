package system

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestTrafficDB(t *testing.T, path string, flushInterval time.Duration) *TrafficDB {
	t.Helper()
	tdb, err := newTrafficDB(path, flushInterval)
	if err != nil {
		t.Fatalf("newTrafficDB: %v", err)
	}
	return tdb
}

func rowCount(t *testing.T, tdb *TrafficDB) int {
	t.Helper()
	var n int
	if err := tdb.db.QueryRow("SELECT COUNT(*) FROM interface_metrics").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func autoVacuumMode(t *testing.T, tdb *TrafficDB) int {
	t.Helper()
	var mode int
	if err := tdb.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	return mode
}

func TestDefaultFlushIntervalIsOneMinute(t *testing.T) {
	tdb, err := NewTrafficDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close()

	if tdb.flushInterval != time.Minute {
		t.Errorf("flushInterval = %v, want 1m", tdb.flushInterval)
	}
}

func TestNewDatabaseUsesIncrementalVacuum(t *testing.T) {
	tdb := newTestTrafficDB(t, filepath.Join(t.TempDir(), "t.db"), time.Hour)
	defer tdb.Close()

	if mode := autoVacuumMode(t, tdb); mode != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (incremental)", mode)
	}
}

func TestLegacyDatabaseIsConvertedAndKeepsItsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// A database as the old code created it: same schema, no auto_vacuum
	legacy, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE interface_metrics (iface TEXT NOT NULL, timestamp INTEGER NOT NULL,
			rx_bytes INTEGER NOT NULL, tx_bytes INTEGER NOT NULL, PRIMARY KEY (iface, timestamp));
		INSERT INTO interface_metrics VALUES ('eth0', ?, 100, 200);`, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	tdb := newTestTrafficDB(t, path, time.Hour)
	defer tdb.Close()

	if mode := autoVacuumMode(t, tdb); mode != 2 {
		t.Errorf("auto_vacuum = %d after opening a legacy database, want 2", mode)
	}
	if n := rowCount(t, tdb); n != 1 {
		t.Errorf("legacy rows after conversion = %d, want 1", n)
	}
}

func TestExpiredHistoryIsPurgedAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")

	first := newTestTrafficDB(t, path, time.Hour)
	old := time.Now().AddDate(0, 0, -(retentionDays + 10)).Unix()
	recent := time.Now().Add(-time.Hour).Unix()
	if _, err := first.db.Exec(`INSERT INTO interface_metrics VALUES ('eth0', ?, 1, 1), ('eth0', ?, 2, 2)`, old, recent); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newTestTrafficDB(t, path, time.Hour)
	defer second.Close()

	if n := rowCount(t, second); n != 1 {
		t.Errorf("rows after startup purge = %d, want 1 (only the recent one)", n)
	}
}

func TestSamplesAreFlushedOnTheInterval(t *testing.T) {
	tdb := newTestTrafficDB(t, filepath.Join(t.TempDir(), "t.db"), 50*time.Millisecond)
	defer tdb.Close()

	tdb.RecordSample("eth0", 10, 20)

	deadline := time.Now().Add(3 * time.Second)
	for rowCount(t, tdb) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sample was not flushed within 3s with a 50ms flush interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSamplesStayBufferedUntilShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")

	tdb := newTestTrafficDB(t, path, time.Hour)
	tdb.RecordSample("eth0", 10, 20)
	tdb.RecordSample("eth1", 30, 40)
	time.Sleep(150 * time.Millisecond)

	if n := rowCount(t, tdb); n != 0 {
		t.Errorf("rows written before the flush interval = %d, want 0 (writes should be batched)", n)
	}

	if err := tdb.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newTestTrafficDB(t, path, time.Hour)
	defer reopened.Close()
	if n := rowCount(t, reopened); n != 2 {
		t.Errorf("rows after shutdown flush = %d, want 2 (nothing buffered should be lost)", n)
	}
}
