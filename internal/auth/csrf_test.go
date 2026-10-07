package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type loggedIn struct {
	cookie *http.Cookie
	csrf   string
}

func newSession(t *testing.T, sm *SessionManager, user string) loggedIn {
	t.Helper()
	rec := httptest.NewRecorder()
	token := sm.CreateSession(user, rec)
	cookie := &http.Cookie{Name: sessionCookieName, Value: token}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	info, ok := sm.Lookup(req)
	if !ok {
		t.Fatal("new session is not valid")
	}
	return loggedIn{cookie: cookie, csrf: info.CSRFToken}
}

func (l loggedIn) request(method, target string, body string, header map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if l.cookie != nil {
		r.AddCookie(l.cookie)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

// handlerCalled reports whether the protected handler ran
func run(sm *SessionManager, r *http.Request) (status int, called bool) {
	rec := httptest.NewRecorder()
	RequireAuth(sm, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})(rec, r)
	return rec.Code, called
}

func TestEachSessionGetsItsOwnUnguessableCSRFToken(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	a := newSession(t, sm, "alice")
	b := newSession(t, sm, "bob")

	if len(a.csrf) < 32 {
		t.Errorf("token %q is too short to be unguessable", a.csrf)
	}
	if a.csrf == b.csrf {
		t.Error("two sessions share a CSRF token")
	}
	// A script that can read the token (it sits in the page) must not learn the session cookie
	if a.csrf == a.cookie.Value || strings.Contains(a.csrf, a.cookie.Value) {
		t.Error("the CSRF token must be independent of the session cookie value")
	}
}

func TestCSRFTokenIsStableForTheLifeOfTheSession(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	s := newSession(t, sm, "alice")

	for i := 0; i < 3; i++ {
		info, ok := sm.Lookup(s.request(http.MethodGet, "/", "", nil))
		if !ok || info.CSRFToken != s.csrf {
			t.Fatalf("lookup %d: token = %q, want %q", i, info.CSRFToken, s.csrf)
		}
	}
}

func TestRequireAuthAllowsReadsWithoutAToken(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	s := newSession(t, sm, "alice")

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if status, called := run(sm, s.request(method, "/api/v1/x", "", nil)); status != http.StatusNoContent || !called {
			t.Errorf("%s: status %d called=%v, want the handler to run", method, status, called)
		}
	}
}

func TestRequireAuthRejectsAnonymousRequestsWith401(t *testing.T) {
	sm, _ := newTestSessions(t, 0)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		status, called := run(sm, loggedIn{}.request(method, "/api/v1/x", "", nil))
		if status != http.StatusUnauthorized || called {
			t.Errorf("%s without a session: status %d called=%v, want 401 and no handler", method, status, called)
		}
	}
}

func TestStateChangingRequestsNeedTheSessionsToken(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	alice := newSession(t, sm, "alice")
	bob := newSession(t, sm, "bob")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		cases := []struct {
			name   string
			header map[string]string
			want   int
		}{
			{"no token", nil, http.StatusForbidden},
			{"empty token", map[string]string{CSRFHeader: ""}, http.StatusForbidden},
			{"wrong token", map[string]string{CSRFHeader: "not-the-token"}, http.StatusForbidden},
			{"another session's token", map[string]string{CSRFHeader: bob.csrf}, http.StatusForbidden},
			{"the session cookie value is not a token", map[string]string{CSRFHeader: alice.cookie.Value}, http.StatusForbidden},
			{"the right token", map[string]string{CSRFHeader: alice.csrf}, http.StatusNoContent},
		}
		for _, c := range cases {
			status, called := run(sm, alice.request(method, "/api/v1/x", "", c.header))
			if status != c.want || called != (c.want == http.StatusNoContent) {
				t.Errorf("%s with %s: status %d called=%v, want %d", method, c.name, status, called, c.want)
			}
		}
	}
}

func TestATokenInTheBodyOrQueryDoesNotCountForAPIRequests(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	s := newSession(t, sm, "alice")

	form := url.Values{CSRFFormField: {s.csrf}}.Encode()
	r := s.request(http.MethodPost, "/api/v1/x", form, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if status, _ := run(sm, r); status != http.StatusForbidden {
		t.Errorf("a form-field token on an API request: status %d, want 403 (APIs take the header only)", status)
	}

	r = s.request(http.MethodPost, "/api/v1/x?"+CSRFFormField+"="+s.csrf, "", nil)
	if status, _ := run(sm, r); status != http.StatusForbidden {
		t.Errorf("a query-string token: status %d, want 403", status)
	}
}

func TestExpiredSessionsAreRejectedBeforeTheTokenIsConsidered(t *testing.T) {
	sm, clock := newTestSessions(t, 5)
	s := newSession(t, sm, "alice")
	clock.advance(time.Hour)

	status, called := run(sm, s.request(http.MethodPost, "/api/v1/x", "", map[string]string{CSRFHeader: s.csrf}))
	if status != http.StatusUnauthorized || called {
		t.Errorf("expired session: status %d called=%v, want 401", status, called)
	}
}

func TestCheckCSRFFormAcceptsTheHiddenFieldOrTheHeader(t *testing.T) {
	sm, _ := newTestSessions(t, 0)
	s := newSession(t, sm, "alice")
	formHeader := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	check := func(r *http.Request) bool { return CheckCSRFForm(httptest.NewRecorder(), r, s.csrf) }

	good := url.Values{CSRFFormField: {s.csrf}}.Encode()
	if !check(s.request(http.MethodPost, "/logout", good, formHeader)) {
		t.Error("the hidden form field was rejected")
	}
	if !check(s.request(http.MethodPost, "/logout", "", map[string]string{CSRFHeader: s.csrf})) {
		t.Error("the header was rejected")
	}

	bad := url.Values{CSRFFormField: {"forged"}}.Encode()
	if check(s.request(http.MethodPost, "/logout", bad, formHeader)) {
		t.Error("a forged form token was accepted")
	}
	if check(s.request(http.MethodPost, "/logout", "", formHeader)) {
		t.Error("a missing token was accepted")
	}
	if check(s.request(http.MethodPost, "/logout", url.Values{CSRFFormField: {""}}.Encode(), formHeader)) {
		t.Error("an empty token was accepted")
	}
}

func TestCheckCSRFNeverAcceptsAnEmptyExpectedToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if CheckCSRF(r, "") {
		t.Error("with no expected token, an empty header must not match")
	}
	r.Header.Set(CSRFHeader, "")
	if CheckCSRF(r, "") {
		t.Error("two empty strings must not count as a valid token")
	}
}

func TestSafeMethod(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		if !SafeMethod(m) {
			t.Errorf("%s should be safe", m)
		}
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "TRACE", "CONNECT", "get", ""} {
		if SafeMethod(m) {
			t.Errorf("%q must need a token", m)
		}
	}
}
