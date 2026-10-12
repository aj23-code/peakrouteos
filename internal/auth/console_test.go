package auth

import (
	"errors"
	"net/http"
	"testing"

	"peakroute/internal/config"
)

type fakeConsole struct {
	sets []string // "name:hash"
	err  error
}

func (f *fakeConsole) Set(name, hash string) error {
	if f.err != nil {
		return f.err
	}
	f.sets = append(f.sets, name+":"+hash)
	return nil
}

// bob is the account that is also the console login.
func consoleLab(t *testing.T) (*lab, *fakeConsole) {
	l := newLab(t)
	fc := &fakeConsole{}
	l.api.Console = fc
	if err := l.mgr.UpdateAuth(func(c *config.AuthConfig) error { c.ConsoleUser = "bob"; return nil }); err != nil {
		t.Fatal(err)
	}
	return l, fc
}

func TestThePasswordOfTheConsoleAccountFollowsToTheConsole(t *testing.T) {
	l, fc := consoleLab(t)
	alice := l.signIn("alice")
	// another administrator sets bob's password
	if r := l.do(&alice, http.MethodPost, "/api/v1/admins/bob/password", `{"password":"bob-new-pass-9","confirm":"bob-new-pass-9"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if len(fc.sets) != 1 {
		t.Fatalf("%v", fc.sets)
	}
	var hash string
	for _, u := range l.mgr.Users() {
		if u.Username == "bob" {
			hash = u.PasswordHash
		}
	}
	if fc.sets[0] != "bob:"+hash || !l.canSignIn("bob", "bob-new-pass-9") {
		t.Fatalf("the console got something else than the web account has: %v", fc.sets)
	}
	// bob changes it himself
	bob := l.signIn("bob")
	if r := l.do(&bob, http.MethodPost, "/api/v1/me/password", `{"current":"bob-new-pass-9","password":"bob-third-pass-1","confirm":"bob-third-pass-1"}`); r.code != 200 || len(fc.sets) != 2 {
		t.Fatalf("%d %s %v", r.code, r.body, fc.sets)
	}
	// the other accounts have nothing to do with the console
	if r := l.do(&alice, http.MethodPost, "/api/v1/admins/carol/password", `{"password":"carol-new-pass-9","confirm":"carol-new-pass-9"}`); r.code != 200 || len(fc.sets) != 2 {
		t.Fatalf("%d %v", r.code, fc.sets)
	}
}

func TestIfTheConsoleCannotBeChangedNeitherIs(t *testing.T) {
	l, fc := consoleLab(t)
	fc.err = errors.New("useradd missing")
	alice := l.signIn("alice")
	if r := l.do(&alice, http.MethodPost, "/api/v1/admins/bob/password", `{"password":"bob-new-pass-9","confirm":"bob-new-pass-9"}`); r.code != 500 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if !l.canSignIn("bob", pwBob) || l.canSignIn("bob", "bob-new-pass-9") {
		t.Fatal("the web password changed although the console's did not")
	}
}

func TestTheConsoleAccountStaysAnAdministrator(t *testing.T) {
	l, _ := consoleLab(t)
	alice := l.signIn("alice")
	if r := l.do(&alice, http.MethodDelete, "/api/v1/admins/bob", ""); r.code != http.StatusConflict {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := l.do(&alice, http.MethodPut, "/api/v1/admins/bob", `{"role":"readonly"}`); r.code != http.StatusConflict {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if l.role("bob") != RoleAdmin {
		t.Fatal("bob changed")
	}
	// the others can still be removed
	if r := l.do(&alice, http.MethodDelete, "/api/v1/admins/carol", ""); r.code != 200 {
		t.Fatalf("%d", r.code)
	}
}
