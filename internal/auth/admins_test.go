package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
)

// The accounts of the lab: alice and bob are administrators, carol may only look.
const (
	pwAlice = "alice-password-1"
	pwBob   = "bob-password-22"
	pwCarol = "carol-password-3"
)

type lab struct {
	t     *testing.T
	dir   string
	mgr   *config.Manager
	sm    *SessionManager
	api   *AdminAPI
	clock *fakeClock
	logs  []string
}

func newLab(t *testing.T) *lab {
	t.Helper()
	dir := t.TempDir()
	mgr, err := config.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash := func(p string) string {
		h, err := config.HashPassword(p)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if err := mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = []config.User{
			{Username: "alice", PasswordHash: hash(pwAlice), Role: RoleAdmin},
			{Username: "bob", PasswordHash: hash(pwBob), Role: RoleAdmin},
			{Username: "carol", PasswordHash: hash(pwCarol), Role: RoleReadOnly},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &lab{t: t, dir: dir, mgr: mgr, clock: &fakeClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}}
	l.sm = NewSessionManager(mgr)
	l.sm.now = l.clock.now
	l.api = &AdminAPI{Cfg: mgr, Sessions: l.sm, Logf: func(f string, a ...any) { l.logs = append(l.logs, strings.TrimSpace(fmt.Sprintf(f, a...))) }}
	return l
}

// signIn starts a session for the account and returns what a browser would send.
func (l *lab) signIn(user string) loggedIn { return newSession(l.t, l.sm, user) }

type reply struct {
	code int
	body string
}

func (r reply) json(v any) {
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		panic(err)
	}
}

func (r reply) admins() adminsResponse { var out adminsResponse; r.json(&out); return out }

func (r reply) fields() map[string]string {
	var out struct {
		Errors []fieldError `json:"errors"`
	}
	r.json(&out)
	m := map[string]string{}
	for _, e := range out.Errors {
		m[e.Field] = e.Message
	}
	return m
}

func (r reply) errorText() string {
	var out struct {
		Error string `json:"error"`
	}
	r.json(&out)
	return out.Error
}

// do sends a request through RequireAuth as the session (nil: not signed in) to the handler the pattern names.
func (l *lab) do(who *loggedIn, method, path, body string) reply {
	l.t.Helper()
	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return RequireAuth(l.sm, h) }
	mux.HandleFunc("/api/v1/admins", wrap(l.api.HandleAdmins))
	mux.HandleFunc("/api/v1/admins/{username}", wrap(l.api.HandleAdmin))
	mux.HandleFunc("/api/v1/admins/{username}/password", wrap(l.api.HandleAdminPassword))
	mux.HandleFunc("/api/v1/admins/{username}/sessions", wrap(l.api.HandleAdminSessions))
	mux.HandleFunc(OwnPasswordPath, wrap(l.api.HandleOwnPassword))
	mux.HandleFunc("/api/v1/session/timeout", wrap(l.api.HandleSessionTimeout))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != nil {
		req.AddCookie(who.cookie)
		req.Header.Set(CSRFHeader, who.csrf)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return reply{rec.Code, rec.Body.String()}
}

func (l *lab) valid(s loggedIn) bool {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(s.cookie)
	_, ok := l.sm.Lookup(r)
	return ok
}

func (l *lab) role(user string) string {
	u, ok := FindUser(l.mgr.Users(), user)
	if !ok {
		return ""
	}
	return u.Role
}

func (l *lab) canSignIn(user, password string) bool {
	return Authenticate(l.mgr.Users(), user, password)
}

func TestAReadOnlyAccountCannotChangeAnything(t *testing.T) {
	l := newLab(t)
	carol, alice := l.signIn("carol"), l.signIn("alice")
	var ran int
	h := RequireAuth(l.sm, func(w http.ResponseWriter, r *http.Request) { ran++; w.WriteHeader(http.StatusNoContent) })
	send := func(s loggedIn, method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(s.cookie)
		r.Header.Set(CSRFHeader, s.csrf)
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Code
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if code := send(carol, m, "/api/v1/firewall/rules"); code != http.StatusForbidden {
			t.Errorf("%s by a read-only account: %d", m, code)
		}
		if code := send(alice, m, "/api/v1/firewall/rules"); code != http.StatusNoContent {
			t.Errorf("%s by an administrator: %d", m, code)
		}
	}
	if ran != 4 {
		t.Fatalf("the handler ran %d times", ran)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if code := send(carol, m, "/api/v1/firewall"); code != http.StatusNoContent {
			t.Errorf("%s by a read-only account: %d", m, code)
		}
	}
	// its own password is the one exception, and only at that path
	if code := send(carol, http.MethodPost, OwnPasswordPath); code != http.StatusNoContent {
		t.Errorf("own password: %d", code)
	}
	if code := send(carol, http.MethodPost, OwnPasswordPath+"/x"); code != http.StatusForbidden {
		t.Errorf("a path that only starts like it: %d", code)
	}
	// the answer says why
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.AddCookie(carol.cookie)
	r.Header.Set(CSRFHeader, carol.csrf)
	h(rec, r)
	if !strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("%s", rec.Body)
	}
}

func TestWhatAnAccountMayDoIsReadAtEveryRequest(t *testing.T) {
	l := newLab(t)
	bob := l.signIn("bob")
	if r := l.do(&bob, http.MethodPut, "/api/v1/session/timeout", `{"minutes":15}`); r.code != http.StatusOK {
		t.Fatalf("bob is an administrator: %d %s", r.code, r.body)
	}
	// demoted: the session he already has stops being allowed to change things, at once
	alice := l.signIn("alice")
	if r := l.do(&alice, http.MethodPut, "/api/v1/admins/bob", `{"role":"readonly"}`); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := l.do(&bob, http.MethodPut, "/api/v1/session/timeout", `{"minutes":30}`); r.code != http.StatusForbidden {
		t.Fatalf("a demoted account still changes things: %d %s", r.code, r.body)
	}
	// removed: the session is no longer a session
	if r := l.do(&alice, http.MethodDelete, "/api/v1/admins/bob", ""); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if l.valid(bob) {
		t.Fatal("the session of a removed account is still valid")
	}
}

func TestAnUnknownRoleInTheFileIsReadOnly(t *testing.T) {
	l := newLab(t)
	l.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = append(c.Users, config.User{Username: "dave", PasswordHash: "x", Role: "superuser"}, config.User{Username: "erin", PasswordHash: "x"})
		return nil
	})
	for _, name := range []string{"dave", "erin"} {
		s := l.signIn(name)
		if r := l.do(&s, http.MethodPut, "/api/v1/session/timeout", `{"minutes":30}`); r.code != http.StatusForbidden {
			t.Errorf("%s: %d", name, r.code)
		}
	}
	if NormalizeRole("admin") != RoleAdmin || NormalizeRole("") != RoleReadOnly || NormalizeRole("ADMIN") != RoleReadOnly || NormalizeRole("readonly") != RoleReadOnly {
		t.Error("NormalizeRole")
	}
	if !CanWrite(RoleAdmin) || CanWrite(RoleReadOnly) || CanWrite("") {
		t.Error("CanWrite")
	}
}

func TestTheListShowsEveryoneToAnAdministratorAndOnlyYourselfToOthers(t *testing.T) {
	l := newLab(t)
	alice := l.signIn("alice")
	l.signIn("bob")
	l.signIn("bob")
	r := l.do(&alice, http.MethodGet, "/api/v1/admins", "")
	if r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	v := r.admins()
	if !v.CanManage || v.You != "alice" || v.Role != RoleAdmin || len(v.Users) != 3 {
		t.Fatalf("%+v", v)
	}
	if v.Users[0].Username != "alice" || !v.Users[0].You || v.Users[1].Username != "bob" || v.Users[2].Username != "carol" || v.Users[2].Role != RoleReadOnly {
		t.Fatalf("yourself first, then by name: %+v", v.Users)
	}
	if v.Users[0].Sessions != 1 || v.Users[1].Sessions != 2 || v.Users[2].Sessions != 0 {
		t.Fatalf("sessions: %+v", v.Users)
	}
	if v.SessionTimeout != 15 || v.TimeoutMin != 1 || v.TimeoutMax != 1440 || v.TimeoutDefault != 15 || v.MinPassword != 8 || v.MaxPasswordBytes != 72 || v.MaxAdmins != 20 {
		t.Fatalf("%+v", v)
	}
	if strings.Contains(r.body, "password_hash") || strings.Contains(r.body, "$2a$") {
		t.Fatalf("a password hash was sent: %s", r.body)
	}
	// a read-only account sees itself
	carol := l.signIn("carol")
	v = l.do(&carol, http.MethodGet, "/api/v1/admins", "").admins()
	if v.CanManage || v.You != "carol" || v.Role != RoleReadOnly || len(v.Users) != 1 || v.Users[0].Username != "carol" {
		t.Fatalf("%+v", v)
	}
	if r := l.do(nil, http.MethodGet, "/api/v1/admins", ""); r.code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", r.code)
	}
}

func TestAddingAnAccount(t *testing.T) {
	l := newLab(t)
	alice := l.signIn("alice")
	r := l.do(&alice, http.MethodPost, "/api/v1/admins", `{"username":" dave ","role":"readonly","password":"dave-password-4","confirm":"dave-password-4"}`)
	if r.code != http.StatusCreated {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if v := r.admins(); len(v.Users) != 4 {
		t.Fatalf("%+v", v)
	}
	if !l.canSignIn("dave", "dave-password-4") || l.role("dave") != RoleReadOnly {
		t.Fatal("the new account does not work as asked")
	}
	raw, _ := os.ReadFile(filepath.Join(l.dir, "auth.json"))
	if strings.Contains(string(raw), "dave-password-4") {
		t.Fatal("the password is stored as it was typed")
	}
	if info, _ := os.Stat(filepath.Join(l.dir, "auth.json")); info.Mode().Perm() != 0600 {
		t.Fatalf("auth.json is %v", info.Mode())
	}
	if !strings.Contains(strings.Join(l.logs, "\n"), `"dave" added by "alice"`) {
		t.Fatalf("not logged: %v", l.logs)
	}
	for _, line := range l.logs {
		if strings.Contains(line, "dave-password-4") {
			t.Fatalf("a password was logged: %s", line)
		}
	}
}

func TestAddingAnAccountIsChecked(t *testing.T) {
	l := newLab(t)
	alice, carol := l.signIn("alice"), l.signIn("carol")
	ok := func(over string) string {
		m := map[string]string{"username": "dave", "role": "admin", "password": "dave-password-4", "confirm": "dave-password-4"}
		var o map[string]string
		json.Unmarshal([]byte(over), &o)
		for k, v := range o {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	cases := []struct {
		name, body, field string
		code              int
	}{
		{"short name", ok(`{"username":"ab"}`), "username", 400},
		{"bad characters", ok(`{"username":"da ve"}`), "username", 400},
		{"starts with a dot", ok(`{"username":".dave"}`), "username", 400},
		{"empty name", ok(`{"username":""}`), "username", 400},
		{"no role", ok(`{"role":""}`), "role", 400},
		{"made up role", ok(`{"role":"owner"}`), "role", 400},
		{"role in capitals", ok(`{"role":"ADMIN"}`), "role", 400},
		{"short password", ok(`{"password":"short","confirm":"short"}`), "password", 400},
		{"empty password", ok(`{"password":"","confirm":""}`), "password", 400},
		{"long password", ok(`{"password":"` + strings.Repeat("a", 73) + `","confirm":"` + strings.Repeat("a", 73) + `"}`), "password", 400},
		{"passwords differ", ok(`{"confirm":"dave-password-5"}`), "confirm", 400},
		{"the name exists", ok(`{"username":"bob"}`), "username", 409},
		{"the name exists in other capitals", ok(`{"username":"Bob"}`), "username", 409},
	}
	for _, c := range cases {
		r := l.do(&alice, http.MethodPost, "/api/v1/admins", c.body)
		if r.code != c.code || (c.field != "" && r.fields()[c.field] == "") {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
	}
	if n := len(l.mgr.Users()); n != 3 {
		t.Fatalf("a refused request added an account: %d accounts", n)
	}
	for _, m := range []struct {
		who  *loggedIn
		body string
		code int
	}{{&carol, ok(""), 403}, {nil, ok(""), 401}, {&alice, `{"username":"dave"`, 400}, {&alice, ok(`{"x":1}`)[:len(ok(""))-1] + `,"extra":1}`, 400}} {
		if r := l.do(m.who, http.MethodPost, "/api/v1/admins", m.body); r.code != m.code {
			t.Errorf("%q: %d %s", m.body, r.code, r.body)
		}
	}
	if r := l.do(&alice, http.MethodPatch, "/api/v1/admins", "{}"); r.code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH: %d", r.code)
	}
	// without the JSON content type
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admins", strings.NewReader(ok("")))
	req.AddCookie(alice.cookie)
	req.Header.Set(CSRFHeader, alice.csrf)
	rec := httptest.NewRecorder()
	l.api.HandleAdmins(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no content type: %d", rec.Code)
	}
}

func TestThereIsALimitToTheAccounts(t *testing.T) {
	l := newLab(t)
	alice := l.signIn("alice")
	l.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		for i := len(c.Users); i < maxAdmins; i++ {
			c.Users = append(c.Users, config.User{Username: "user" + string(rune('a'+i)) + "xx", PasswordHash: "x", Role: RoleReadOnly})
		}
		return nil
	})
	r := l.do(&alice, http.MethodPost, "/api/v1/admins", `{"username":"onemore","role":"readonly","password":"onemore-pass-1","confirm":"onemore-pass-1"}`)
	if r.code != http.StatusConflict || len(l.mgr.Users()) != maxAdmins {
		t.Fatalf("%d %s (%d accounts)", r.code, r.body, len(l.mgr.Users()))
	}
}

func TestChangingARole(t *testing.T) {
	l := newLab(t)
	alice, carol := l.signIn("alice"), l.signIn("carol")
	if r := l.do(&alice, http.MethodPut, "/api/v1/admins/carol", `{"role":"admin"}`); r.code != http.StatusOK || l.role("carol") != RoleAdmin {
		t.Fatalf("%d %s", r.code, r.body)
	}
	// she can change things now, with the session she already had
	if r := l.do(&carol, http.MethodPut, "/api/v1/session/timeout", `{"minutes":20}`); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := l.do(&alice, http.MethodPut, "/api/v1/admins/carol", `{"role":"readonly"}`); r.code != http.StatusOK || l.role("carol") != RoleReadOnly {
		t.Fatalf("%d %s", r.code, r.body)
	}
	for _, c := range []struct {
		name, path, body string
		code             int
	}{
		{"your own role", "/api/v1/admins/alice", `{"role":"readonly"}`, 409},
		{"an account that is not there", "/api/v1/admins/nobody", `{"role":"admin"}`, 404},
		{"a name that cannot be one", "/api/v1/admins/a", `{"role":"admin"}`, 404},
		{"a made up role", "/api/v1/admins/bob", `{"role":"god"}`, 400},
		{"no role", "/api/v1/admins/bob", `{}`, 400},
		{"an unknown field", "/api/v1/admins/bob", `{"role":"admin","password":"x"}`, 400},
	} {
		if r := l.do(&alice, http.MethodPut, c.path, c.body); r.code != c.code {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
	}
	if l.role("alice") != RoleAdmin || l.role("bob") != RoleAdmin {
		t.Fatal("a refused change was made")
	}
	if r := l.do(&carol, http.MethodPut, "/api/v1/admins/bob", `{"role":"readonly"}`); r.code != http.StatusForbidden {
		t.Errorf("a read-only account changing a role: %d", r.code)
	}
}

func TestRemovingAnAccount(t *testing.T) {
	l := newLab(t)
	alice, carol := l.signIn("alice"), l.signIn("carol")
	l.signIn("carol")
	if r := l.do(&alice, http.MethodDelete, "/api/v1/admins/carol", ""); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	} else if v := r.admins(); len(v.Users) != 2 {
		t.Fatalf("%+v", v)
	}
	if l.role("carol") != "" || l.canSignIn("carol", pwCarol) || l.valid(carol) || l.sm.ActiveSessions()["carol"] != 0 {
		t.Fatal("the account or its sessions are still there")
	}
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"yourself", "/api/v1/admins/alice", 409},
		{"an account that is not there", "/api/v1/admins/carol", 404},
		{"a name that cannot be one", "/api/v1/admins/x", 404},
	} {
		if r := l.do(&alice, http.MethodDelete, c.path, ""); r.code != c.code {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
	}
	bob := l.signIn("bob")
	l.mgr.UpdateAuth(func(c *config.AuthConfig) error { c.Users[1].Role = RoleReadOnly; return nil }) // bob can only look
	if r := l.do(&bob, http.MethodDelete, "/api/v1/admins/alice", ""); r.code != http.StatusForbidden {
		t.Errorf("a read-only account removing an administrator: %d", r.code)
	}
	if l.role("alice") != RoleAdmin {
		t.Fatal("alice was removed")
	}
}

func TestTheLastAdministratorStays(t *testing.T) {
	cfg := func() *config.AuthConfig {
		return &config.AuthConfig{Users: []config.User{
			{Username: "alice", Role: RoleAdmin}, {Username: "bob", Role: RoleAdmin}, {Username: "carol", Role: RoleReadOnly}, {Username: "old"}}}
	}
	c := cfg()
	// two administrators removing each other at once: the second request finds only one left
	if err := changeAccount(c, "alice", true, ""); err != nil {
		t.Fatal(err)
	}
	if err := changeAccount(c, "bob", true, ""); err != errLastAdmin {
		t.Fatalf("%v", err)
	}
	if err := changeAccount(c, "bob", false, RoleReadOnly); err != errLastAdmin {
		t.Fatalf("%v", err)
	}
	if err := changeAccount(c, "bob", false, RoleAdmin); err != nil {
		t.Fatalf("an administrator staying one: %v", err)
	}
	if len(c.Users) != 3 || c.Users[0].Username != "bob" {
		t.Fatalf("%+v", c.Users)
	}
	// others can go whatever their number
	if err := changeAccount(c, "carol", true, ""); err != nil {
		t.Fatal(err)
	}
	if err := changeAccount(c, "nobody", true, ""); err != errNoSuchUser {
		t.Fatalf("%v", err)
	}
	if err := changeAccount(c, "nobody", false, RoleAdmin); err != errNoSuchUser {
		t.Fatalf("%v", err)
	}
	// an account with no role is read-only, so it is not counted
	c = cfg()
	c.Users = []config.User{{Username: "alice", Role: RoleAdmin}, {Username: "old"}}
	if err := changeAccount(c, "alice", true, ""); err != errLastAdmin {
		t.Fatalf("%v", err)
	}
	// the slice the caller kept is not damaged by a removal
	c = cfg()
	before := append([]config.User(nil), c.Users...)
	keep := c.Users
	changeAccount(c, "alice", true, "")
	for i := range before {
		if keep[i] != before[i] {
			t.Fatalf("removal rewrote the old slice: %+v", keep)
		}
	}
}

func TestSettingAnotherAccountsPassword(t *testing.T) {
	l := newLab(t)
	alice := l.signIn("alice")
	carol := l.signIn("carol")
	r := l.do(&alice, http.MethodPost, "/api/v1/admins/carol/password", `{"password":"carol-new-pass-9","confirm":"carol-new-pass-9"}`)
	if r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if !l.canSignIn("carol", "carol-new-pass-9") || l.canSignIn("carol", pwCarol) {
		t.Fatal("the password did not change")
	}
	if l.valid(carol) || !l.valid(alice) {
		t.Fatal("her sessions end, and yours does not")
	}
	if strings.Contains(strings.Join(l.logs, "\n"), "carol-new-pass-9") {
		t.Fatal("the password was logged")
	}
	for _, c := range []struct {
		name, path, body string
		code             int
		field            string
	}{
		{"yourself", "/api/v1/admins/alice/password", `{"password":"alice-new-pass-9","confirm":"alice-new-pass-9"}`, 409, ""},
		{"nobody", "/api/v1/admins/nobody/password", `{"password":"nobody-pass-99","confirm":"nobody-pass-99"}`, 404, ""},
		{"short", "/api/v1/admins/carol/password", `{"password":"short","confirm":"short"}`, 400, "password"},
		{"differ", "/api/v1/admins/carol/password", `{"password":"carol-new-pass-9","confirm":"carol-new-pass-8"}`, 400, "confirm"},
	} {
		r := l.do(&alice, http.MethodPost, c.path, c.body)
		if r.code != c.code || (c.field != "" && r.fields()[c.field] == "") {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
	}
	if !l.canSignIn("alice", pwAlice) {
		t.Fatal("alice's password changed")
	}
	carol2 := l.signIn("carol")
	if r := l.do(&carol2, http.MethodPost, "/api/v1/admins/alice/password", `{"password":"alice-new-pass-9","confirm":"alice-new-pass-9"}`); r.code != http.StatusForbidden {
		t.Errorf("a read-only account setting a password: %d", r.code)
	}
	if !l.canSignIn("alice", pwAlice) {
		t.Fatal("alice's password changed")
	}
}

func TestChangingYourOwnPassword(t *testing.T) {
	l := newLab(t)
	l.api.Attempts = NewRateLimiter(5, time.Minute)
	here, elsewhere := l.signIn("alice"), l.signIn("alice")
	bob := l.signIn("bob")
	body := func(cur, pw, conf string) string {
		b, _ := json.Marshal(map[string]string{"current": cur, "password": pw, "confirm": conf})
		return string(b)
	}
	// the wrong current password
	r := l.do(&here, http.MethodPost, OwnPasswordPath, body("wrong-password", "alice-newer-pw-1", "alice-newer-pw-1"))
	if r.code != http.StatusBadRequest || r.fields()["current"] == "" || !l.canSignIn("alice", pwAlice) {
		t.Fatalf("%d %s", r.code, r.body)
	}
	for _, c := range []struct{ name, body, field string }{
		{"short", body(pwAlice, "short", "short"), "password"},
		{"differ", body(pwAlice, "alice-newer-pw-1", "alice-newer-pw-2"), "confirm"},
		{"the same", body(pwAlice, pwAlice, pwAlice), "password"},
	} {
		if r := l.do(&here, http.MethodPost, OwnPasswordPath, c.body); r.code != http.StatusBadRequest || r.fields()[c.field] == "" {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
	}
	if !l.canSignIn("alice", pwAlice) || !l.valid(elsewhere) {
		t.Fatal("a refused change had an effect")
	}
	r = l.do(&here, http.MethodPost, OwnPasswordPath, body(pwAlice, "alice-newer-pw-1", "alice-newer-pw-1"))
	if r.code != http.StatusOK || !strings.Contains(r.body, `"ended":1`) {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if !l.canSignIn("alice", "alice-newer-pw-1") || l.canSignIn("alice", pwAlice) {
		t.Fatal("the password did not change")
	}
	if !l.valid(here) || l.valid(elsewhere) || !l.valid(bob) {
		t.Fatal("the other sessions of alice end, this one and bob's do not")
	}
	for _, line := range l.logs {
		if strings.Contains(line, "alice-newer-pw-1") || strings.Contains(line, pwAlice) {
			t.Fatalf("a password was logged: %s", line)
		}
	}
	// a read-only account may do it, and it is not signed in without a session
	carol := l.signIn("carol")
	if r := l.do(&carol, http.MethodPost, OwnPasswordPath, body(pwCarol, "carol-newer-pw-1", "carol-newer-pw-1")); r.code != http.StatusOK || !l.canSignIn("carol", "carol-newer-pw-1") {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := l.do(nil, http.MethodPost, OwnPasswordPath, body("a", "b", "b")); r.code != http.StatusUnauthorized {
		t.Fatalf("%d", r.code)
	}
	if r := l.do(&carol, http.MethodGet, OwnPasswordPath, ""); r.code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", r.code)
	}
}

func TestGuessingTheCurrentPasswordIsLimited(t *testing.T) {
	l := newLab(t)
	l.api.Attempts = NewRateLimiter(5, time.Minute)
	alice, bob := l.signIn("alice"), l.signIn("bob")
	try := func(s *loggedIn, cur string) int {
		return l.do(s, http.MethodPost, OwnPasswordPath, `{"current":"`+cur+`","password":"some-new-pass-1","confirm":"some-new-pass-1"}`).code
	}
	for i := 0; i < 5; i++ {
		if code := try(&alice, "guess"); code != http.StatusBadRequest {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	// the sixth is refused without looking at the password, even the right one
	if code := try(&alice, pwAlice); code != http.StatusTooManyRequests || !l.canSignIn("alice", pwAlice) {
		t.Fatalf("%d", code)
	}
	// another account has its own count
	if code := try(&bob, "guess"); code != http.StatusBadRequest {
		t.Fatalf("%d", code)
	}
}

func TestSigningAnAccountOutEverywhere(t *testing.T) {
	l := newLab(t)
	here := l.signIn("alice")
	elsewhere := l.signIn("alice")
	c1, c2 := l.signIn("carol"), l.signIn("carol")
	// yourself: everywhere else
	if r := l.do(&here, http.MethodDelete, "/api/v1/admins/alice/sessions", ""); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if !l.valid(here) || l.valid(elsewhere) {
		t.Fatal("yourself: this session stays, the others end")
	}
	// someone else: all of it
	if r := l.do(&here, http.MethodDelete, "/api/v1/admins/carol/sessions", ""); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if l.valid(c1) || l.valid(c2) || !l.valid(here) {
		t.Fatal("carol is signed out everywhere")
	}
	if r := l.do(&here, http.MethodDelete, "/api/v1/admins/nobody/sessions", ""); r.code != http.StatusNotFound {
		t.Errorf("nobody: %d", r.code)
	}
	carol := l.signIn("carol")
	if r := l.do(&carol, http.MethodDelete, "/api/v1/admins/alice/sessions", ""); r.code != http.StatusForbidden || !l.valid(here) {
		t.Errorf("a read-only account signing an administrator out: %d", r.code)
	}
}

func TestTheSessionTimeout(t *testing.T) {
	l := newLab(t)
	alice := l.signIn("alice")
	r := l.do(&alice, http.MethodPut, "/api/v1/session/timeout", `{"minutes":45}`)
	if r.code != http.StatusOK || r.admins().SessionTimeout != 45 || l.mgr.SessionTimeout() != 45*time.Minute {
		t.Fatalf("%d %s", r.code, r.body)
	}
	raw, _ := os.ReadFile(filepath.Join(l.dir, "auth.json"))
	if !strings.Contains(string(raw), `"session_timeout_minutes": 45`) {
		t.Fatalf("not saved: %s", raw)
	}
	// a session that exists already follows the new time from its next request on
	l.clock.advance(10 * time.Minute)
	if !l.valid(alice) {
		t.Fatal("expired before its timeout")
	}
	l.clock.advance(44 * time.Minute)
	if !l.valid(alice) {
		t.Fatal("expired before the new timeout")
	}
	l.clock.advance(46 * time.Minute)
	if l.valid(alice) {
		t.Fatal("still valid after the timeout")
	}
	alice = l.signIn("alice")
	for _, bad := range []string{`{"minutes":0}`, `{"minutes":-5}`, `{"minutes":1441}`, `{}`, `{"minutes":"15"}`, `{"minutes":15,"x":1}`} {
		if r := l.do(&alice, http.MethodPut, "/api/v1/session/timeout", bad); r.code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, r.code)
		}
	}
	for _, ok := range []string{`{"minutes":1}`, `{"minutes":1440}`} {
		if r := l.do(&alice, http.MethodPut, "/api/v1/session/timeout", ok); r.code != http.StatusOK {
			t.Errorf("%s: %d", ok, r.code)
		}
	}
	carol := l.signIn("carol")
	if r := l.do(&carol, http.MethodPut, "/api/v1/session/timeout", `{"minutes":30}`); r.code != http.StatusForbidden || l.mgr.SessionTimeout() != 1440*time.Minute {
		t.Errorf("a read-only account: %d", r.code)
	}
}

func TestTheAccountsSurviveARestart(t *testing.T) {
	l := newLab(t)
	alice := l.signIn("alice")
	l.do(&alice, http.MethodPost, "/api/v1/admins", `{"username":"dave","role":"readonly","password":"dave-password-4","confirm":"dave-password-4"}`)
	l.do(&alice, http.MethodPut, "/api/v1/admins/bob", `{"role":"readonly"}`)
	l.do(&alice, http.MethodDelete, "/api/v1/admins/carol", "")
	again, err := config.NewManager(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.ReloadAll(); err != nil {
		t.Fatal(err)
	}
	users := again.Users()
	got := map[string]string{}
	for _, u := range users {
		got[u.Username] = u.Role
	}
	if len(got) != 3 || got["alice"] != RoleAdmin || got["bob"] != RoleReadOnly || got["dave"] != RoleReadOnly {
		t.Fatalf("%v", got)
	}
	if !Authenticate(users, "dave", "dave-password-4") {
		t.Fatal("dave cannot sign in")
	}
}

// the handlers check the role themselves too, not only the wrapper in front of them
func TestTheHandlersRefuseAReadOnlyAccountOnTheirOwn(t *testing.T) {
	l := newLab(t)
	carol := l.signIn("carol")
	call := func(h http.HandlerFunc, method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("username", "bob")
		req.AddCookie(carol.cookie)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	for name, code := range map[string]int{
		"add":      call(l.api.HandleAdmins, http.MethodPost, "/x", `{"username":"dave","role":"admin","password":"dave-password-4","confirm":"dave-password-4"}`),
		"role":     call(l.api.HandleAdmin, http.MethodPut, "/x", `{"role":"readonly"}`),
		"delete":   call(l.api.HandleAdmin, http.MethodDelete, "/x", ""),
		"password": call(l.api.HandleAdminPassword, http.MethodPost, "/x", `{"password":"bob-hacked-pw-1","confirm":"bob-hacked-pw-1"}`),
		"sessions": call(l.api.HandleAdminSessions, http.MethodDelete, "/x", ""),
		"timeout":  call(l.api.HandleSessionTimeout, http.MethodPut, "/x", `{"minutes":5}`),
	} {
		if code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(l.mgr.Users()) != 3 || l.role("bob") != RoleAdmin || !l.canSignIn("bob", pwBob) || l.mgr.SessionTimeout() != 15*time.Minute {
		t.Fatal("something changed")
	}
}

func TestTheListIsSortedWithoutRegardToCapitals(t *testing.T) {
	l := newLab(t)
	l.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = append(c.Users, config.User{Username: "Zoe", PasswordHash: "x", Role: RoleReadOnly}, config.User{Username: "amy", PasswordHash: "x", Role: RoleReadOnly})
		return nil
	})
	alice := l.signIn("alice")
	var names []string
	for _, u := range l.do(&alice, http.MethodGet, "/api/v1/admins", "").admins().Users {
		names = append(names, u.Username)
	}
	if strings.Join(names, ",") != "alice,amy,bob,carol,Zoe" {
		t.Fatalf("%v", names)
	}
}

func TestASessionNeedsItsAccountEvenWhenTheFileIsChangedByHand(t *testing.T) {
	l := newLab(t)
	carol := l.signIn("carol")
	if !l.valid(carol) {
		t.Fatal("not valid to begin with")
	}
	// the account disappears from the file without going through the page (so nothing ended its sessions)
	l.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = c.Users[:2]
		return nil
	})
	if l.valid(carol) {
		t.Fatal("a session outlived its account")
	}
}

func TestSessionsThatTimedOutAreNotCounted(t *testing.T) {
	l := newLab(t)
	l.signIn("carol") // never used again
	l.clock.advance(14 * time.Minute)
	alice := l.signIn("alice")
	l.clock.advance(2 * time.Minute) // carol's session is over, not yet swept away
	var carol adminUser
	for _, u := range l.do(&alice, http.MethodGet, "/api/v1/admins", "").admins().Users {
		if u.Username == "carol" {
			carol = u
		}
	}
	if carol.Sessions != 0 {
		t.Fatalf("%+v", carol)
	}
}
