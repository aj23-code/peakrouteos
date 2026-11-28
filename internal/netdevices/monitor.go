package netdevices

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"

	"peakroute/internal/fsutil"
)

// Monitor is the access point's side of being watched: one read-only token, of which only the SHA-256 is kept, that lets the router that
// watches it ask GET /api/v1/ap/status without a session. The token is 256 random bits, so a hash is enough; it opens nothing else (the
// middleware only stands in for a sign-in on the one path it wraps) and is revoked from the access point's own page.
type Monitor struct {
	mu   sync.Mutex
	path string
	hash string // hex SHA-256, "" when no token is issued
}

// OpenMonitor loads the token's hash from path (a missing file means no token).
func OpenMonitor(path string) (*Monitor, error) {
	m := &Monitor{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	m.hash = f.Hash
	return m, nil
}

func hashOf(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Enabled says whether a token is issued.
func (m *Monitor) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hash != ""
}

func (m *Monitor) save() error {
	b, _ := json.Marshal(struct {
		Hash string `json:"hash"`
	}{m.hash})
	return fsutil.WriteFileAtomic(m.path, b, 0600)
}

// Generate issues a new token (the old one stops working) and returns it: it is shown once and never kept.
func (m *Monitor) Generate() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.hash
	m.hash = hashOf(tok)
	if err := m.save(); err != nil {
		m.hash = old
		return "", err
	}
	return tok, nil
}

// Revoke takes the token away.
func (m *Monitor) Revoke() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.hash
	m.hash = ""
	if err := m.save(); err != nil {
		m.hash = old
		return err
	}
	return nil
}

// Check says whether token is the one issued.
func (m *Monitor) Check(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hash == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashOf(token)), []byte(m.hash)) == 1
}

// Allow wraps a read-only handler: a request with the token goes straight through; any other goes to signIn (the usual session check).
func (m *Monitor) Allow(signIn func(http.HandlerFunc) http.HandlerFunc, h http.HandlerFunc) http.HandlerFunc {
	guarded := signIn(h)
	return func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && r.Method == http.MethodGet && m.Check(strings.TrimSpace(tok)) {
			h(w, r)
			return
		}
		guarded(w, r)
	}
}

// HandleMonitoring: GET says whether a token is issued; POST issues a new one (answering with it, once); DELETE revokes it.
func (m *Monitor) HandleMonitoring(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"enabled": m.Enabled()})
	case http.MethodPost:
		tok, err := m.Generate()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "token": tok})
	case http.MethodDelete:
		if err := m.Revoke(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
