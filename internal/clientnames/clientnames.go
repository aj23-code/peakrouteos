// Package clientnames keeps the nicknames a person gives the devices in the client list, by hardware address. A nickname is only a label
// on the router's own pages: it changes nothing in the system, so it is saved at once (no draft, no commit) to a file of its own next
// to the other state, and it never reaches DNS or DHCP (a reserved address has its own host name for that).
package clientnames

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"peakroute/internal/auth"
	"peakroute/internal/config"
	"peakroute/internal/fsutil"
)

const (
	// MaxNameRunes is the longest nickname, in characters.
	MaxNameRunes = 40
	// MaxNames is how many devices can have one.
	MaxNames = 2000
)

// Store holds the nicknames.
type Store struct {
	path string

	mu    sync.RWMutex
	names map[string]string // hardware address (lower case, colons) -> nickname
}

// New makes a store that keeps its nicknames in the file (empty: in memory only).
func New(path string) *Store { return &Store{path: path, names: map[string]string{}} }

// Load reads what was kept. A missing or unreadable file starts empty.
func (s *Store) Load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for mac, name := range m {
		if n, ok := config.NormalizeMAC(mac); ok {
			if clean, err := CleanName(name); err == nil && clean != "" {
				s.names[n] = clean
			}
		}
	}
}

// Get returns the nickname of a device ("" when it has none).
func (s *Store) Get(mac string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[strings.ToLower(strings.ReplaceAll(mac, "-", ":"))]
}

// CleanName trims a nickname and checks it: at most MaxNameRunes characters, no control characters or line breaks. Empty is allowed
// (it means no nickname).
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxNameRunes {
		return "", errors.New("use at most 40 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", errors.New("use letters, digits, spaces and punctuation only")
		}
	}
	return name, nil
}

// Set gives a device a nickname, or takes it away (an empty name). The file is written before the nickname counts.
func (s *Store) Set(mac, name string) error {
	m, ok := config.NormalizeMAC(mac)
	if !ok {
		return errors.New("that is not a device hardware address")
	}
	clean, err := CleanName(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(s.names)+1)
	for k, v := range s.names {
		next[k] = v
	}
	if clean == "" {
		delete(next, m)
	} else {
		if _, had := next[m]; !had && len(next) >= MaxNames {
			return errors.New("there are too many nicknames: remove some first")
		}
		next[m] = clean
	}
	if s.path != "" {
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(s.path, b, 0600); err != nil {
			return errors.New("the nickname could not be saved")
		}
	}
	s.names = next
	return nil
}

// All returns a copy of every nickname, by hardware address.
func (s *Store) All() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.names))
	for k, v := range s.names {
		out[k] = v
	}
	return out
}

// ParseFile reads the nicknames of a saved file (or of a backup): a JSON object of hardware address to name. Entries that are not
// valid (a bad address, a name that is too long or has control characters, more than MaxNames) are left out and counted, so that one bad
// entry never costs the rest; only something that is not such an object is an error.
func ParseFile(b []byte) (names map[string]string, skipped int, err error) {
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, 0, errors.New("the nicknames are not in the form this router writes them")
	}
	names = map[string]string{}
	for mac, name := range raw {
		m, ok := config.NormalizeMAC(mac)
		clean, nerr := CleanName(name)
		if !ok || nerr != nil || clean == "" || len(names) >= MaxNames {
			skipped++
			continue
		}
		names[m] = clean
	}
	return names, skipped, nil
}

// Replace makes the given nicknames the only ones (checked as ParseFile does, so use its result). The file is written before they count.
func (s *Store) Replace(names map[string]string) error {
	next := make(map[string]string, len(names))
	for mac, name := range names {
		m, ok := config.NormalizeMAC(mac)
		clean, err := CleanName(name)
		if !ok || err != nil || clean == "" || len(next) >= MaxNames {
			continue
		}
		next[m] = clean
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(s.path, b, 0600); err != nil {
			return errors.New("the nicknames could not be saved")
		}
	}
	s.names = next
	return nil
}

type setRequest struct {
	Name string `json:"name"`
}

// HandleName: PUT /api/v1/clients/{mac}/name {"name": "Kitchen tablet"} sets the nickname of a device; an empty name removes it. Writing is
// for administrators (the session layer refuses anyone else before this is reached).
func (s *Store) HandleName(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		fail(http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req setRequest
	if err := auth.DecodeJSONStrict(w, r, &req, 4<<10); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	mac := r.PathValue("mac")
	if err := s.Set(mac, req.Name); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	m, _ := config.NormalizeMAC(mac)
	json.NewEncoder(w).Encode(map[string]string{"mac": m, "name": s.Get(m)})
}
