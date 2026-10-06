package auth

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

const (
	MinPasswordLength = 8
	// bcrypt only uses the first 72 bytes of a password (and rejects longer input)
	MaxPasswordBytes = 72
)

// Starts with a letter or digit, then letters, digits, '.', '_' or '-' (3-32 chars total)
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{2,31}$`)

// ValidateUsername checks a new admin username against the allowed format.
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return errors.New("Username must be 3-32 characters, start with a letter or number, and use only letters, numbers, '.', '_' or '-'.")
	}
	return nil
}

// ValidateNewPassword checks a new password and its confirmation entry.
func ValidateNewPassword(password, confirm string) error {
	if password == "" {
		return errors.New("Password cannot be empty.")
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("Password must be at least %d characters.", MinPasswordLength)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("Password must be at most %d bytes long.", MaxPasswordBytes)
	}
	if password != confirm {
		return errors.New("Passwords do not match.")
	}
	return nil
}
