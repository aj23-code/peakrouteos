package auth

import (
	"testing"
	"time"

	"peakroute/internal/config"
)

func testUsers(t *testing.T) []config.User {
	t.Helper()
	hash, err := config.HashPassword("correct-horse-9")
	if err != nil {
		t.Fatal(err)
	}
	return []config.User{{Username: "admin", PasswordHash: hash, Role: "admin"}}
}

func TestAuthenticate(t *testing.T) {
	users := testUsers(t)

	cases := []struct {
		name             string
		username, secret string
		want             bool
	}{
		{"correct credentials", "admin", "correct-horse-9", true},
		{"wrong password", "admin", "wrong-password", false},
		{"empty password", "admin", "", false},
		{"unknown user", "nobody", "correct-horse-9", false},
		{"empty username", "", "correct-horse-9", false},
		{"username is case sensitive", "Admin", "correct-horse-9", false},
		{"username prefix is not a match", "adm", "correct-horse-9", false},
	}
	for _, c := range cases {
		if got := Authenticate(users, c.username, c.secret); got != c.want {
			t.Errorf("%s: Authenticate(%q, ...) = %v, want %v", c.name, c.username, got, c.want)
		}
	}
}

func TestAuthenticateWithNoUsers(t *testing.T) {
	if Authenticate(nil, "admin", "anything") {
		t.Error("no users configured, but authentication succeeded")
	}
}

func TestAuthenticateFindsTheRightUserAmongSeveral(t *testing.T) {
	a, _ := config.HashPassword("alice-password-1")
	b, _ := config.HashPassword("bob-password-22")
	users := []config.User{{Username: "alice", PasswordHash: a}, {Username: "bob", PasswordHash: b}}

	if !Authenticate(users, "bob", "bob-password-22") {
		t.Error("bob should authenticate with his own password")
	}
	if Authenticate(users, "bob", "alice-password-1") {
		t.Error("bob must not authenticate with alice's password")
	}
}

// An unknown user used to return immediately while a wrong password paid for a bcrypt check, so
// response time revealed which usernames exist. Both must now cost about the same.
func TestUnknownUserCostsAboutAsMuchAsAWrongPassword(t *testing.T) {
	users := testUsers(t)

	fastest := func(username string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			Authenticate(users, username, "wrong-password")
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	wrongPassword := fastest("admin")
	unknownUser := fastest("nobody")

	if unknownUser < wrongPassword/2 {
		t.Errorf("unknown user took %v but a wrong password took %v: the difference leaks which usernames exist",
			unknownUser, wrongPassword)
	}
}
