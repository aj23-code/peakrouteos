package console

import (
	"context"
	"errors"
	"os/user"
	"strings"
	"testing"
)

type call struct {
	stdin string
	args  []string
}

type lab struct {
	calls  []call
	users  map[string]string // name -> uid
	groups map[string]bool
	fail   string // a command that fails
	sys    System
}

func newLab() *lab {
	l := &lab{users: map[string]string{"root": "0", "daemon": "1", "www-data": "33", "nobody": "65534", "oldadmin": "1000"}, groups: map[string]bool{"root": true, "sudo": true, "staff": true}}
	l.sys = System{
		Run: func(ctx context.Context, stdin, name string, args ...string) (string, error) {
			l.calls = append(l.calls, call{stdin, append([]string{name}, args...)})
			if name == l.fail {
				return "boom with secret-hash", errors.New("exit 1")
			}
			return "", nil
		},
		Lookup: func(name string) (*user.User, error) {
			if uid, ok := l.users[name]; ok {
				return &user.User{Username: name, Uid: uid}, nil
			}
			return nil, user.UnknownUserError(name)
		},
		LookupGroup: func(name string) (*user.Group, error) {
			if l.groups[name] {
				return &user.Group{Name: name}, nil
			}
			return nil, user.UnknownGroupError(name)
		},
	}
	return l
}

func TestWhatCanBeAConsoleName(t *testing.T) {
	for name, ok := range map[string]bool{"alec": true, "net-admin": true, "a_b": true, "admin1": true, "ab": false, "Alec": false, "1alec": false, "-alec": false, "a.b": false,
		"alec smith": false, "": false, strings.Repeat("a", 33): false, strings.Repeat("a", 32): true, "é-user": false, "a;rm": false} {
		if got := ValidName(name) == nil; got != ok {
			t.Errorf("%q: %v", name, got)
		}
	}
}

func TestANameTheSystemUsesIsRefused(t *testing.T) {
	l := newLab()
	for _, n := range []string{"root", "daemon", "www-data", "nobody", "oldadmin", "sudo", "staff", "Alec"} {
		if err := l.sys.Check(n); err == nil {
			t.Errorf("%q was accepted", n)
		}
	}
	if err := l.sys.Check("alec"); err != nil {
		t.Fatal(err)
	}
	if len(l.calls) != 0 {
		t.Fatal("checking ran a command")
	}
}

func TestMakingTheAccountAndGivingItThePasswordHash(t *testing.T) {
	l := newLab()
	hash := "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	if err := l.sys.Set("alec", hash); err != nil {
		t.Fatal(err)
	}
	if len(l.calls) != 2 {
		t.Fatalf("%+v", l.calls)
	}
	add := strings.Join(l.calls[0].args, " ")
	if add != "useradd --create-home --shell /bin/bash --groups sudo alec" {
		t.Fatalf("%s", add)
	}
	if strings.Join(l.calls[1].args, " ") != "chpasswd -e" || l.calls[1].stdin != "alec:"+hash+"\n" {
		t.Fatalf("%+v", l.calls[1])
	}
	// the hash is never on a command line
	for _, c := range l.calls {
		if strings.Contains(strings.Join(c.args, " "), hash) {
			t.Fatal("the hash is in the arguments")
		}
	}
}

func TestAnAccountThatIsThereOnlyGetsTheNewPassword(t *testing.T) {
	l := newLab()
	if err := l.sys.Set("oldadmin", "hash"); err != nil {
		t.Fatal(err)
	}
	if len(l.calls) != 1 || l.calls[0].args[0] != "chpasswd" {
		t.Fatalf("%+v", l.calls)
	}
}

func TestTheSystemsOwnAccountsAreNeverChanged(t *testing.T) {
	l := newLab()
	for _, n := range []string{"root", "daemon", "www-data", "nobody"} {
		if l.sys.Set(n, "hash") == nil || l.sys.Remove(n) == nil {
			t.Errorf("%q was touched", n)
		}
	}
	if len(l.calls) != 0 {
		t.Fatalf("%+v", l.calls)
	}
	if l.sys.Set("alec", "") == nil || l.sys.Set("BAD", "h") == nil {
		t.Fatal("accepted")
	}
}

func TestAFailingCommandIsAnErrorWithoutTheOutput(t *testing.T) {
	l := newLab()
	l.fail = "useradd"
	err := l.sys.Set("alec", "hash")
	if err == nil || strings.Contains(err.Error(), "secret-hash") {
		t.Fatalf("%v", err)
	}
	if len(l.calls) != 1 {
		t.Fatal("went on after the failure")
	}
	l = newLab()
	l.fail = "chpasswd"
	if l.sys.Set("alec", "hash") == nil {
		t.Fatal("no error")
	}
}

func TestRemoving(t *testing.T) {
	l := newLab()
	if err := l.sys.Remove("oldadmin"); err != nil || strings.Join(l.calls[0].args, " ") != "userdel --remove oldadmin" {
		t.Fatalf("%v %+v", err, l.calls)
	}
	l = newLab()
	if err := l.sys.Remove("ghost"); err != nil || len(l.calls) != 0 {
		t.Fatalf("%v %+v", err, l.calls)
	}
}

func TestADevMachineTouchesNothing(t *testing.T) {
	if (Dev{}).Set("alec", "h") != nil || (Dev{}).Set("Bad", "h") == nil || (Dev{}).Check("Bad") == nil || (Dev{}).Remove("alec") != nil {
		t.Fatal("dev stand-in")
	}
}
