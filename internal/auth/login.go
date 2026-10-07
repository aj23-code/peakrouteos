package auth

import (
	"crypto/subtle"

	"peakroute/internal/config"
)

// dummyHash is checked when the username doesn't exist, so an unknown user costs the same
// bcrypt comparison as a wrong password and response time doesn't reveal which usernames exist.
// It is made with config.HashPassword so it always has the same cost as real hashes.
var dummyHash = func() string {
	h, err := config.HashPassword("peakroute-dummy-password-for-timing")
	if err != nil {
		panic("auth: cannot create dummy password hash: " + err.Error())
	}
	return h
}()

// Authenticate reports whether username and password match one of users. It always performs
// exactly one bcrypt comparison, whatever the input.
func Authenticate(users []config.User, username, password string) bool {
	hash := dummyHash
	found := false

	// Visit every user without stopping early so the work doesn't depend on who matched
	for _, u := range users {
		if subtle.ConstantTimeCompare([]byte(u.Username), []byte(username)) == 1 {
			hash = u.PasswordHash
			found = true
		}
	}

	passwordOK := config.CheckPasswordHash(password, hash)
	return found && passwordOK
}
