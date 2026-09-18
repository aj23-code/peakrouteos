package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

type Session struct {
	Username  string
	ExpiresAt time.Time
}

type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]Session
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]Session),
	}
}

func (sm *SessionManager) CreateSession(username string, w http.ResponseWriter) string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	bytes := make([]byte, 16)
	rand.Read(bytes)
	token := hex.EncodeToString(bytes)

	//Use this to set the amount of time a created session lasts
	expiresAt := time.Now().Add(15 * time.Minute)
	sm.sessions[token] = Session{
		Username:  username,
		ExpiresAt: expiresAt,
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "peakroute_session",
		Value:    token,
		Expires:  expiresAt,
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
	})

	return token
}

func (sm *SessionManager) ValidateSession(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("peakroute_session")
	if err != nil {
		return "", false
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessions[cookie.Value]
	if !exists {
		return "", false
	}

	// Check if session has expired
	if time.Now().After(session.ExpiresAt) {
		delete(sm.sessions, cookie.Value)
		return "", false
	}

	// ROLLING TIMEOUT: Extend session by another 15 minutes on active use
	session.ExpiresAt = time.Now().Add(15 * time.Minute)
	sm.sessions[cookie.Value] = session

	return session.Username, true
}

// DestroySession invalidates the active session in memory and clears the client cookie.
func (sm *SessionManager) DestroySession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		sm.mu.Lock()
		delete(sm.sessions, cookie.Value) // Remove from in-memory session store
		sm.mu.Unlock()
	}

	// Overwrite the client cookie with an expired MaxAge to purge it from the browser
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		       HttpOnly: true,
		       Secure:   true,
		       SameSite: http.SameSiteStrictMode,
	})
}
