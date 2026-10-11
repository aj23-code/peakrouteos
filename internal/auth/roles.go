package auth

import (
	"strings"

	"peakroute/internal/config"
)

// The two kinds of account. An administrator can change everything; a read-only account can look at everything the
// pages show and change nothing (except its own password).
const (
	RoleAdmin    = "admin"
	RoleReadOnly = "readonly"
)

// NormalizeRole turns what is stored into one of the two roles. Anything unknown is read-only: a damaged or hand-edited
// file must never hand out more than it says.
func NormalizeRole(role string) string {
	if role == RoleAdmin {
		return RoleAdmin
	}
	return RoleReadOnly
}

// CanWrite reports whether the role may change things.
func CanWrite(role string) bool { return role == RoleAdmin }

// FindUser returns the account with the exact user name.
func FindUser(users []config.User, username string) (config.User, bool) {
	for _, u := range users {
		if u.Username == username {
			return u, true
		}
	}
	return config.User{}, false
}

// sameName compares user names the way duplicates are judged: "Admin" and "admin" are the same person to a human.
func sameName(a, b string) bool { return strings.EqualFold(a, b) }
