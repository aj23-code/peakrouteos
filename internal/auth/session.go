package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"peakroute/internal/config"
)

const sessionCookieName = "peakroute_session"

type Session struct {
	Username  string
	CSRFToken string // separate from the cookie so a script that can read it still can't steal the session
	ExpiresAt time.Time
}

// SessionInfo is what a valid session tells a handler about its user.
type SessionInfo struct {
	Username  string
	CSRFToken string
	Role      string // what the account may do now (read from the configuration at every request, so a change counts at once)
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]Session
	mgr      *config.Manager
	now      func() time.Time // replaced in tests
}

func NewSessionManager(mgr *config.Manager) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]Session),
		mgr:      mgr,
		now:      time.Now,
	}
}

// timeout is the idle time before a session expires, from the config (with a default when unset).
func (sm *SessionManager) timeout() time.Duration {
	if sm.mgr == nil {
		return config.DefaultSessionTimeoutMinutes * time.Minute
	}
	return sm.mgr.SessionTimeout()
}

func (sm *SessionManager) CreateSession(username string, w http.ResponseWriter) string {
	timeout := sm.timeout()

	sm.mu.Lock()
	defer sm.mu.Unlock()

	token := randomToken()

	sm.sessions[token] = Session{
		Username:  username,
		CSRFToken: randomToken() + randomToken(),
		ExpiresAt: sm.now().Add(timeout),
	}

	// No Expires/MaxAge: this is a browser-session cookie. Expiry is enforced here on the server
	// and slides with activity; a fixed cookie lifetime would log users out mid-use no matter
	// how the server-side timeout is configured.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		HttpOnly: true,
		Secure:   true,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
	})

	return token
}

// ValidateSession reports who the request belongs to, extending the session on use.
func (sm *SessionManager) ValidateSession(r *http.Request) (string, bool) {
	info, ok := sm.Lookup(r)
	return info.Username, ok
}

// Lookup validates the request's session (extending it on use) and returns its details,
// including the CSRF token the page must send with state-changing requests.
func (sm *SessionManager) Lookup(r *http.Request) (SessionInfo, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return SessionInfo{}, false
	}

	timeout := sm.timeout()

	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessions[cookie.Value]
	if !exists {
		return SessionInfo{}, false
	}

	now := sm.now()

	// Check if session has expired
	if now.After(session.ExpiresAt) {
		delete(sm.sessions, cookie.Value)
		return SessionInfo{}, false
	}

	// The account must still exist, and what it may do is what the configuration says now
	role := RoleAdmin
	if sm.mgr != nil {
		user, ok := FindUser(sm.mgr.Users(), session.Username)
		if !ok {
			delete(sm.sessions, cookie.Value)
			return SessionInfo{}, false
		}
		role = NormalizeRole(user.Role)
	}

	// ROLLING TIMEOUT: extend the session on active use
	session.ExpiresAt = now.Add(timeout)
	sm.sessions[cookie.Value] = session

	return SessionInfo{Username: session.Username, CSRFToken: session.CSRFToken, Role: role}, true
}

// DestroySession invalidates the active session in memory and clears the client cookie.
func (sm *SessionManager) DestroySession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		sm.mu.Lock()
		delete(sm.sessions, cookie.Value)
		sm.mu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// PurgeExpired removes sessions that have timed out and returns how many it dropped. Without
// this, sessions that are never used again (closed tabs) would stay in memory indefinitely.
func (sm *SessionManager) PurgeExpired() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := sm.now()
	purged := 0
	for token, session := range sm.sessions {
		if now.After(session.ExpiresAt) {
			delete(sm.sessions, token)
			purged++
		}
	}
	return purged
}

// StartJanitor purges expired sessions every interval until ctx is cancelled.
func (sm *SessionManager) StartJanitor(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sm.PurgeExpired()
			}
		}
	}()
}

// RevokeUser ends every session of the account, except the one the request belongs to (when keep is not nil). It returns
// how many it ended.
func (sm *SessionManager) RevokeUser(username string, keep *http.Request) int {
	keepToken := ""
	if keep != nil {
		if c, err := keep.Cookie(sessionCookieName); err == nil {
			keepToken = c.Value
		}
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	n := 0
	for token, s := range sm.sessions {
		if s.Username == username && token != keepToken {
			delete(sm.sessions, token)
			n++
		}
	}
	return n
}

// RevokeAll ends every session (after the administrators were replaced, no one is signed in any more). It returns how many it ended.
func (sm *SessionManager) RevokeAll() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	n := len(sm.sessions)
	sm.sessions = map[string]Session{}
	return n
}

// ActiveSessions counts the sessions that have not timed out, by account.
func (sm *SessionManager) ActiveSessions() map[string]int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := sm.now()
	out := map[string]int{}
	for _, s := range sm.sessions {
		if !now.After(s.ExpiresAt) {
			out[s.Username]++
		}
	}
	return out
}
