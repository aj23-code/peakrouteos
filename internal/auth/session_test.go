package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"peakroute/internal/config"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestSessions(t *testing.T, timeoutMinutes int) (*SessionManager, *fakeClock) {
	t.Helper()

	mgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if timeoutMinutes != 0 {
		if err := mgr.UpdateAuth(func(cfg *config.AuthConfig) error {
			cfg.SessionTimeoutMinutes = timeoutMinutes
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	clock := &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	sm := NewSessionManager(mgr)
	sm.now = clock.now
	return sm, clock
}

// login creates a session and returns a request carrying its cookie
func login(t *testing.T, sm *SessionManager, user string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	token := sm.CreateSession(user, rec)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	return req, rec
}

func TestSessionLifecycle(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	req, _ := login(t, sm, "admin")

	if user, ok := sm.ValidateSession(req); !ok || user != "admin" {
		t.Fatalf("ValidateSession = (%q, %v), want (admin, true)", user, ok)
	}

	sm.DestroySession(httptest.NewRecorder(), req)
	if _, ok := sm.ValidateSession(req); ok {
		t.Error("session still valid after logout")
	}
}

func TestSessionRejectsMissingAndUnknownCookies(t *testing.T) {
	sm, _ := newTestSessions(t, 0)

	if _, ok := sm.ValidateSession(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Error("request without a cookie was accepted")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"})
	if _, ok := sm.ValidateSession(req); ok {
		t.Error("unknown token was accepted")
	}
}

func TestSessionExpiresAfterTheConfiguredIdleTime(t *testing.T) {
	sm, clock := newTestSessions(t, 30)
	req, _ := login(t, sm, "admin")

	clock.advance(29 * time.Minute)
	if _, ok := sm.ValidateSession(req); !ok {
		t.Fatal("session expired after 29 minutes with a 30 minute timeout")
	}

	clock.advance(31 * time.Minute) // idle for longer than the timeout since the last use
	if _, ok := sm.ValidateSession(req); ok {
		t.Error("session still valid after being idle for 31 minutes with a 30 minute timeout")
	}
}

func TestSessionTimeoutSlidesWithActivity(t *testing.T) {
	sm, clock := newTestSessions(t, 10)
	req, _ := login(t, sm, "admin")

	// Active every 8 minutes for an hour: far longer than the 10 minute timeout in total
	for i := 0; i < 8; i++ {
		clock.advance(8 * time.Minute)
		if _, ok := sm.ValidateSession(req); !ok {
			t.Fatalf("active session expired after %d minutes", (i+1)*8)
		}
	}
}

func TestDefaultTimeoutAppliesWhenNothingIsConfigured(t *testing.T) {
	sm, clock := newTestSessions(t, 0)
	req, _ := login(t, sm, "admin")

	clock.advance(14 * time.Minute)
	if _, ok := sm.ValidateSession(req); !ok {
		t.Fatal("session expired before the 15 minute default")
	}
	clock.advance(16 * time.Minute)
	if _, ok := sm.ValidateSession(req); ok {
		t.Error("session outlived the 15 minute default")
	}
}

// The browser must not drop the cookie on its own schedule: a fixed Expires made every login
// end after the original lifetime even though the server-side timeout slides.
func TestSessionCookieIsASessionCookieWithSecureAttributes(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	_, rec := login(t, sm, "admin")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if !c.Expires.IsZero() || c.MaxAge != 0 {
		t.Errorf("cookie has a fixed lifetime (Expires=%v MaxAge=%d); expiry must be enforced server-side", c.Expires, c.MaxAge)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie attributes = HttpOnly:%v Secure:%v SameSite:%v Path:%q", c.HttpOnly, c.Secure, c.SameSite, c.Path)
	}
}

func TestPurgeExpiredOnlyRemovesTimedOutSessions(t *testing.T) {
	sm, clock := newTestSessions(t, 10)

	oldReq, _ := login(t, sm, "old")
	clock.advance(8 * time.Minute)
	freshReq, _ := login(t, sm, "fresh")
	clock.advance(5 * time.Minute) // old is now 13 min idle, fresh is 5

	if n := sm.PurgeExpired(); n != 1 {
		t.Errorf("PurgeExpired removed %d sessions, want 1", n)
	}
	if len(sm.sessions) != 1 {
		t.Errorf("%d sessions remain, want 1", len(sm.sessions))
	}
	if _, ok := sm.ValidateSession(freshReq); !ok {
		t.Error("the active session was purged")
	}
	if _, ok := sm.ValidateSession(oldReq); ok {
		t.Error("the expired session survived")
	}
}

func TestJanitorRemovesAbandonedSessionsInTheBackground(t *testing.T) {
	sm, clock := newTestSessions(t, 1)
	for i := 0; i < 5; i++ {
		login(t, sm, "abandoned")
	}
	clock.advance(time.Hour) // nobody ever comes back

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sm.StartJanitor(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for {
		sm.mu.Lock()
		remaining := len(sm.sessions)
		sm.mu.Unlock()
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor left %d expired sessions in memory", remaining)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
