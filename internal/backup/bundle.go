package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the version of what is inside a backup (the shape of network.json and auth.json). A router refuses a backup with a
// higher number than it knows and migrates a lower one; it goes up when a change to those files is not one an older router could read.
const SchemaVersion = 1

// The sections of a backup, as the page offers them.
const (
	Settings     = "settings"
	Admins       = "admins"
	Certificates = "certificates"
	History      = "history"
)

// The files a backup may hold, by path inside it. Nothing else is accepted when a backup is opened.
const (
	FileNetwork = "settings/network.json"
	FileAuth    = "admins/auth.json"
	FileSys     = "admins/sys.json"
	certPrefix  = "certificates/"
	histPrefix  = "history/"
)

// CertFiles are the files of the certificates section, by name in the certificate directory.
var CertFiles = []string{"ca.crt", "ca.key", "tls.crt", "tls.key", "custom", "extra-names.json"}

// HistoryFiles are the databases of the history section, by name in the configuration directory.
var HistoryFiles = []string{"traffic_history.db", "wan_health.db", "firewall_log.db"}

const (
	maxUnpacked = 1 << 30 // what a backup may unpack to
	maxManifest = 1 << 20
)

// FileInfo is one file of a backup, with the checksum that is checked when it is opened.
type FileInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PortsInfo is which ports the settings use, so that the page can tell whether this router has them without reading the settings.
type PortsInfo struct {
	Wan    string   `json:"wan"`
	Lan    []string `json:"lan"`
	Radios []string `json:"radios"`
}

// Manifest describes a backup.
type Manifest struct {
	Format   int        `json:"format"`
	Schema   int        `json:"schema"`
	Created  int64      `json:"created"` // Unix seconds
	Hostname string     `json:"hostname"`
	Software string     `json:"software"`
	Sections []string   `json:"sections"`
	Files    []FileInfo `json:"files"`
	Ports    PortsInfo  `json:"ports"`
}

// Has says whether the backup has a section.
func (m Manifest) Has(section string) bool {
	for _, s := range m.Sections {
		if s == section {
			return true
		}
	}
	return false
}

// Bundle is what a backup holds: the manifest and the files by path.
type Bundle struct {
	Manifest Manifest
	Files    map[string][]byte
}

func allowedPath(p string) bool {
	switch p {
	case FileNetwork, FileAuth, FileSys:
		return true
	}
	if rest, ok := strings.CutPrefix(p, certPrefix); ok {
		for _, f := range CertFiles {
			if f == rest {
				return true
			}
		}
	}
	if rest, ok := strings.CutPrefix(p, histPrefix); ok {
		for _, f := range HistoryFiles {
			if f == rest {
				return true
			}
		}
	}
	return false
}

func sectionOf(p string) string {
	switch {
	case strings.HasPrefix(p, "settings/"):
		return Settings
	case strings.HasPrefix(p, "admins/"):
		return Admins
	case strings.HasPrefix(p, certPrefix):
		return Certificates
	}
	return History
}

// Encode writes the bundle as a gzipped tar: the manifest first, then every file (the manifest is made here, from the files).
func (b *Bundle) Encode() ([]byte, error) {
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		if !allowedPath(p) {
			return nil, fmt.Errorf("%q does not belong in a backup", p)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	m := b.Manifest
	m.Format, m.Schema = FormatVersion, SchemaVersion
	m.Files = nil
	seen := map[string]bool{}
	for _, p := range paths {
		sum := sha256.Sum256(b.Files[p])
		m.Files = append(m.Files, FileInfo{Path: p, Size: int64(len(b.Files[p])), SHA256: hex.EncodeToString(sum[:])})
		seen[sectionOf(p)] = true
	}
	m.Sections = nil
	for _, s := range []string{Settings, Admins, Certificates, History} {
		if seen[s] {
			m.Sections = append(m.Sections, s)
		}
	}
	mj, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), ModTime: time.Unix(m.Created, 0), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put("manifest.json", mj); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := put(p, b.Files[p]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var errDamaged = errors.New("the backup is damaged")

// Decode reads a bundle and checks it strictly: the manifest first, only files it lists and that are allowed, each with the size and
// checksum it says, and nothing twice.
func Decode(data []byte) (*Bundle, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errDamaged
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxUnpacked))
	b := &Bundle{Files: map[string][]byte{}}
	first := true
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errDamaged
		}
		if h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxUnpacked || path.Clean(h.Name) != h.Name {
			return nil, errDamaged
		}
		limit := h.Size
		if first {
			if h.Name != "manifest.json" || h.Size > maxManifest {
				return nil, errDamaged
			}
		} else if !allowedPath(h.Name) {
			return nil, fmt.Errorf("the backup holds a file that does not belong in one (%q)", h.Name)
		}
		total += limit
		if total > maxUnpacked {
			return nil, errDamaged
		}
		body, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil || int64(len(body)) != limit {
			return nil, errDamaged
		}
		if first {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&b.Manifest); err != nil {
				return nil, errDamaged
			}
			first = false
			continue
		}
		if _, dup := b.Files[h.Name]; dup {
			return nil, errDamaged
		}
		b.Files[h.Name] = body
	}
	if first {
		return nil, errDamaged
	}
	m := b.Manifest
	if m.Format != FormatVersion {
		return nil, fmt.Errorf("the backup has an unknown layout (%d)", m.Format)
	}
	if m.Schema > SchemaVersion {
		return nil, fmt.Errorf("this backup was made by a newer version of PeakRoute OS (settings version %d, this router understands %d); update this router first", m.Schema, SchemaVersion)
	}
	if m.Schema < 1 {
		return nil, errDamaged
	}
	listed := map[string]FileInfo{}
	for _, f := range m.Files {
		if _, dup := listed[f.Path]; dup {
			return nil, errDamaged
		}
		listed[f.Path] = f
	}
	if len(listed) != len(b.Files) {
		return nil, errDamaged
	}
	for p, body := range b.Files {
		f, ok := listed[p]
		sum := sha256.Sum256(body)
		if !ok || f.Size != int64(len(body)) || f.SHA256 != hex.EncodeToString(sum[:]) {
			return nil, errDamaged
		}
	}
	return b, nil
}

// ---- reading the router into a bundle -----------------------------------------------------------------------------------

// Source says where a backup is read from.
type Source struct {
	ConfigDir string // network.json, auth.json, sys.json and the databases
	CertDir   string
	History   bool
	Hostname  string
	Software  string
	Ports     PortsInfo
	Now       time.Time
}

func readIfThere(p string) ([]byte, bool, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// snapshotDB copies a SQLite database that is in use to a consistent file (VACUUM INTO), without stopping whatever has it open.
func snapshotDB(p string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "prbackup")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(p)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := filepath.Join(tmp, "copy.db")
	if _, err := db.Exec("VACUUM INTO ?", out); err != nil {
		return nil, fmt.Errorf("copying %s: %w", filepath.Base(p), err)
	}
	return os.ReadFile(out)
}

// Collect reads what a backup holds from the router. The settings are the committed ones (what is running), never a draft.
func Collect(src Source) (*Bundle, error) {
	b := &Bundle{Files: map[string][]byte{}}
	b.Manifest = Manifest{Created: src.Now.Unix(), Hostname: src.Hostname, Software: src.Software, Ports: src.Ports}
	for p, file := range map[string]string{FileNetwork: "network.json", FileAuth: "auth.json", FileSys: "sys.json"} {
		body, ok, err := readIfThere(filepath.Join(src.ConfigDir, file))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		if ok {
			b.Files[p] = body
		}
	}
	for _, f := range CertFiles {
		body, ok, err := readIfThere(filepath.Join(src.CertDir, f))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		if ok {
			b.Files[certPrefix+f] = body
		}
	}
	if src.History {
		for _, f := range HistoryFiles {
			p := filepath.Join(src.ConfigDir, f)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			body, err := snapshotDB(p)
			if err != nil {
				return nil, err
			}
			b.Files[histPrefix+f] = body
		}
	}
	if len(b.Files) == 0 {
		return nil, errors.New("there is nothing to back up")
	}
	return b, nil
}

// File is the name a backup is offered under.
func FileName(hostname string, t time.Time) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(hostname) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			sb.WriteRune(r)
		case r == '.' || r == '_' || r == ' ':
			sb.WriteByte('-')
		}
	}
	name := strings.Trim(sb.String(), "-")
	if name == "" {
		name = "router"
	}
	return fmt.Sprintf("peakroute-%s-%s.prbackup", name, t.Format("2006-01-02"))
}
