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
	ExpiresAt time.Time
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

	bytes := make([]byte, 16)
	_, _ = rand.Read(bytes)
	token := hex.EncodeToString(bytes)

	sm.sessions[token] = Session{
		Username:  username,
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

func (sm *SessionManager) ValidateSession(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}

	timeout := sm.timeout()

	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessions[cookie.Value]
	if !exists {
		return "", false
	}

	now := sm.now()

	// Check if session has expired
	if now.After(session.ExpiresAt) {
		delete(sm.sessions, cookie.Value)
		return "", false
	}

	// ROLLING TIMEOUT: extend the session on active use
	session.ExpiresAt = now.Add(timeout)
	sm.sessions[cookie.Value] = session

	return session.Username, true
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
