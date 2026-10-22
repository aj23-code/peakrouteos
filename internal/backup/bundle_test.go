package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var when = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func sample() *Bundle {
	return &Bundle{Manifest: Manifest{Created: when.Unix(), Hostname: "gateway", Software: "v1", Ports: PortsInfo{Wan: "eth0", Lan: []string{"eth1"}}}, Files: map[string][]byte{
		FileNetwork:             []byte(`{"wan_interface":"eth0"}`),
		FileAuth:                []byte(`{"users":[]}`),
		FileSys:                 []byte(`{"hostname":"gateway"}`),
		"certificates/ca.crt":   []byte("CA"),
		"certificates/ca.key":   []byte("KEY"),
		"history/wan_health.db": []byte("DB"),
	}}
}

// rawTar makes a gzipped tar of exactly the entries given, to test what Decode refuses.
func rawTar(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0600, Size: int64(len(e[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(e[1]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestABundleRoundTrips(t *testing.T) {
	enc, err := sample().Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	m := got.Manifest
	if m.Format != FormatVersion || m.Schema != SchemaVersion || m.Hostname != "gateway" || m.Created != when.Unix() || m.Ports.Wan != "eth0" || len(m.Files) != 6 ||
		strings.Join(m.Sections, ",") != "settings,admins,certificates,history" || !m.Has(History) || m.Has("nope") {
		t.Fatalf("%+v", m)
	}
	for p, body := range sample().Files {
		if !bytes.Equal(got.Files[p], body) {
			t.Errorf("%s: %q", p, got.Files[p])
		}
	}
	// without some sections they are not listed
	b := sample()
	delete(b.Files, "history/wan_health.db")
	enc, _ = b.Encode()
	got, _ = Decode(enc)
	if got.Manifest.Has(History) || len(got.Files) != 5 {
		t.Fatalf("%+v", got.Manifest)
	}
	// the same bundle encodes to the same bytes
	a1, _ := sample().Encode()
	a2, _ := sample().Encode()
	if !bytes.Equal(a1, a2) {
		t.Fatal("the encoding is not stable")
	}
	if _, err := (&Bundle{Files: map[string][]byte{"../etc/passwd": []byte("x")}}).Encode(); err == nil {
		t.Fatal("a path outside was encoded")
	}
}

func TestADamagedOrForgedBundleIsRefused(t *testing.T) {
	good := func() []byte { b, _ := sample().Encode(); return b }()
	manifest := func(files string, mod func(m map[string]any)) string {
		m := map[string]any{"format": 1, "schema": 1, "created": 1, "hostname": "h", "software": "s", "sections": []string{"settings"}, "ports": map[string]any{"wan": "", "lan": nil, "radios": nil},
			"files": json.RawMessage(files)}
		if mod != nil {
			mod(m)
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	sum := "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae" // sha256("foo")
	okFiles := `[{"path":"settings/network.json","size":3,"sha256":"` + sum + `"}]`
	cases := map[string][]byte{
		"not gzip":                   []byte("hello"),
		"empty":                      rawTar(t, nil),
		"no manifest first":          rawTar(t, [][2]string{{"settings/network.json", "foo"}}),
		"manifest not json":          rawTar(t, [][2]string{{"manifest.json", "{"}}),
		"unknown manifest key":       rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, func(m map[string]any) { m["extra"] = 1 })}, {"settings/network.json", "foo"}}),
		"a file not listed":          rawTar(t, [][2]string{{"manifest.json", manifest(`[]`, nil)}, {"settings/network.json", "foo"}}),
		"a file missing":             rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, nil)}}),
		"a wrong checksum":           rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, nil)}, {"settings/network.json", "bar"}}),
		"a wrong size":               rawTar(t, [][2]string{{"manifest.json", manifest(strings.Replace(okFiles, `"size":3`, `"size":4`, 1), nil)}, {"settings/network.json", "foo"}}),
		"a path outside":             rawTar(t, [][2]string{{"manifest.json", manifest(`[{"path":"../x","size":3,"sha256":"`+sum+`"}]`, nil)}, {"../x", "foo"}}),
		"an unknown file":            rawTar(t, [][2]string{{"manifest.json", manifest(`[{"path":"etc/passwd","size":3,"sha256":"`+sum+`"}]`, nil)}, {"etc/passwd", "foo"}}),
		"a path that is not clean":   rawTar(t, [][2]string{{"manifest.json", manifest(`[]`, nil)}, {"settings/../settings/network.json", "foo"}}),
		"a file twice":               rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, nil)}, {"settings/network.json", "foo"}, {"settings/network.json", "foo"}}),
		"the same file listed twice": rawTar(t, [][2]string{{"manifest.json", manifest(strings.TrimSuffix(okFiles, "]")+","+strings.TrimPrefix(okFiles, "["), nil)}, {"settings/network.json", "foo"}}),
		"a layout of the future":     rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, func(m map[string]any) { m["format"] = 9 })}, {"settings/network.json", "foo"}}),
		"a schema of the future":     rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, func(m map[string]any) { m["schema"] = SchemaVersion + 1 })}, {"settings/network.json", "foo"}}),
		"a schema of zero":           rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, func(m map[string]any) { m["schema"] = 0 })}, {"settings/network.json", "foo"}}),
		"cut off":                    good[:len(good)/2],
	}
	for name, data := range cases {
		if _, err := Decode(data); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// the one that is right
	if _, err := Decode(rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, nil)}, {"settings/network.json", "foo"}})); err != nil {
		t.Fatalf("the control case: %v", err)
	}
	// a newer schema says so in words
	_, err := Decode(rawTar(t, [][2]string{{"manifest.json", manifest(okFiles, func(m map[string]any) { m["schema"] = SchemaVersion + 1 })}, {"settings/network.json", "foo"}}))
	if err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("%v", err)
	}
	// a link is no file
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	if _, err := Decode(buf.Bytes()); err == nil {
		t.Fatal("a link was accepted")
	}
}

func TestCollectingFromARouter(t *testing.T) {
	cfg, certs := t.TempDir(), filepath.Join(t.TempDir(), "certs")
	os.MkdirAll(certs, 0700)
	w := func(dir, name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w(cfg, "network.json", `{"wan_interface":"eth0"}`)
	w(cfg, "network.json.staged", `{"wan_interface":"eth9"}`) // a draft is not part of a backup
	w(cfg, "auth.json", `{"users":[]}`)
	w(cfg, "sys.json", `{}`)
	w(cfg, "client_presence.json", `{}`)
	w(cfg, "configured", `1`)
	w(certs, "ca.crt", "CA")
	w(certs, "ca.key", "KEY")
	w(certs, "stray.txt", "x")
	b, err := Collect(Source{ConfigDir: cfg, CertDir: certs, Hostname: "Gate Way.home", Software: "v2", Now: when, Ports: PortsInfo{Wan: "eth0"}})
	if err != nil {
		t.Fatal(err)
	}
	// the nicknames are always in it, empty when there are none
	if string(b.Files[FileNames]) != "{}" {
		t.Fatalf("nicknames: %q", b.Files[FileNames])
	}
	if len(b.Files) != 6 || string(b.Files[FileNetwork]) != `{"wan_interface":"eth0"}` || b.Files["certificates/stray.txt"] != nil || b.Files[histPrefix+"wan_health.db"] != nil {
		t.Fatalf("%v", keys(b.Files))
	}
	enc, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decode(enc); err != nil || got.Manifest.Hostname != "Gate Way.home" || got.Manifest.Software != "v2" {
		t.Fatalf("%v", err)
	}
	if _, err := Collect(Source{ConfigDir: t.TempDir(), CertDir: t.TempDir(), Now: when}); err == nil {
		t.Fatal("a backup of nothing")
	}
	for in, want := range map[string]string{"Gate Way.home": "peakroute-gate-way-home-2026-10-11.prbackup", "": "peakroute-router-2026-10-11.prbackup", "../../etc": "peakroute-etc-2026-10-11.prbackup", "ÄÖ": "peakroute-router-2026-10-11.prbackup", "a/b\\c": "peakroute-abc-2026-10-11.prbackup"} {
		if got := FileName(in, when); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func keys(m map[string][]byte) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestADatabaseInUseIsCopiedConsistently(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "wan_health.db")
	// a database in WAL mode with an open connection that has written rows no checkpoint has moved to the main file
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t (n INTEGER); INSERT INTO t VALUES (1),(2),(3)"); err != nil {
		t.Fatal(err)
	}
	body, err := snapshotDB(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	os.WriteFile(copyPath, body, 0600)
	c, err := sql.Open("sqlite", copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var n int
	if err := c.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d rows (%v)", n, err)
	}
	// through Collect: history only when asked for
	certs := t.TempDir()
	os.WriteFile(filepath.Join(cfg, "auth.json"), []byte("{}"), 0600)
	without, _ := Collect(Source{ConfigDir: cfg, CertDir: certs, Now: when})
	with, err := Collect(Source{ConfigDir: cfg, CertDir: certs, History: true, Now: when})
	if err != nil || without.Files[histPrefix+"wan_health.db"] != nil || len(with.Files[histPrefix+"wan_health.db"]) == 0 {
		t.Fatalf("%v", err)
	}
	if _, err := snapshotDB(filepath.Join(cfg, "nothing.db")); err == nil {
		t.Fatal("a database that is not there")
	}
}
