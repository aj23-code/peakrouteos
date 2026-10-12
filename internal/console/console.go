// Package console makes the administrator of the first-run wizard the login of the machine's console as well: a Linux account of the
// same name with the same password (docs/setup.md). The password is handed over as the hash the web interface keeps (bcrypt, which
// the system's crypt understands), so the account never sees the password and the two cannot differ.
package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"time"
)

// Accounts makes and changes the console login.
type Accounts interface {
	// Check says whether a name can be made into a new console account (nil) or why not.
	Check(name string) error
	// Set makes the account if it is not there and gives it the password hash.
	Set(name, hash string) error
	// Remove deletes the account and its home directory.
	Remove(name string) error
}

var nameRule = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)

// ValidName is the rule for the name of a console account: what Linux accepts everywhere, in lower case.
func ValidName(name string) error {
	if !nameRule.MatchString(name) {
		return errors.New("The console login needs a name of 3 to 32 lower-case letters, numbers, '-' or '_', starting with a letter.")
	}
	return nil
}

// Runner runs a command with the text as its standard input.
type Runner func(ctx context.Context, stdin, name string, args ...string) (string, error)

func execRunner(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// People's accounts are 1000 to 59999 on Debian; anything outside (the system's own, and nobody at 65534) is never touched.
const (
	minUID = 1000
	maxUID = 59999
)

func isPerson(u *user.User) bool {
	uid, err := strconv.Atoi(u.Uid)
	return err == nil && uid >= minUID && uid <= maxUID
}

// System is the real thing: useradd, chpasswd and userdel.
type System struct {
	Run   Runner // nil: the real commands
	Group string // the group that may use sudo ("sudo" if empty)

	// Lookup and LookupGroup are the system's (nil: os/user); tests replace them.
	Lookup      func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func (s System) run(stdin, name string, args ...string) error {
	run := s.Run
	if run == nil {
		run = execRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := run(ctx, stdin, name, args...); err != nil {
		log.Printf("[-] %s failed: %v: %s", name, err, out)
		return fmt.Errorf("%s failed", name)
	}
	return nil
}

func (s System) lookup(name string) (*user.User, error) {
	if s.Lookup != nil {
		return s.Lookup(name)
	}
	return user.Lookup(name)
}

func (s System) lookupGroup(name string) (*user.Group, error) {
	if s.LookupGroup != nil {
		return s.LookupGroup(name)
	}
	return user.LookupGroup(name)
}

func (s System) group() string {
	if s.Group != "" {
		return s.Group
	}
	return "sudo"
}

// Check refuses a name the system already uses, as an account or as a group (useradd makes a group of the same name).
func (s System) Check(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if _, err := s.lookup(name); err == nil {
		return errors.New("The system already has an account with that name: choose another.")
	}
	if _, err := s.lookupGroup(name); err == nil {
		return errors.New("The system already uses that name: choose another.")
	}
	return nil
}

func (s System) Set(name, hash string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if hash == "" {
		return errors.New("no password")
	}
	u, err := s.lookup(name)
	switch {
	case err == nil:
		// only an account of a person is ever changed, never one of the system's
		if !isPerson(u) {
			return errors.New("That is an account of the system.")
		}
	default:
		if _, notThere := err.(user.UnknownUserError); !notThere {
			return fmt.Errorf("looking up the account: %w", err)
		}
		if err := s.run("", "useradd", "--create-home", "--shell", "/bin/bash", "--groups", s.group(), name); err != nil {
			return err
		}
	}
	// chpasswd -e takes a hash; standard input keeps it off the command line
	return s.run(name+":"+hash+"\n", "chpasswd", "-e")
}

func (s System) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	u, err := s.lookup(name)
	if err != nil {
		return nil // nothing to remove
	}
	if !isPerson(u) {
		return errors.New("That is an account of the system.")
	}
	return s.run("", "userdel", "--remove", name)
}

// Dev stands in on a developer's machine: it only says what it would do (never touches the host's accounts).
type Dev struct{}

func (Dev) Check(name string) error { return ValidName(name) }
func (Dev) Set(name, hash string) error {
	log.Printf("[dev] would make the console account %q with the password of the web account", name)
	return ValidName(name)
}
func (Dev) Remove(name string) error {
	log.Printf("[dev] would remove the console account %q", name)
	return nil
}
