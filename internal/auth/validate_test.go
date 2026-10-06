package auth

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"typical", "admin", true},
		{"with separators", "net.admin_01-a", true},
		{"minimum length", "abc", true},
		{"maximum length", strings.Repeat("a", 32), true},
		{"empty", "", false},
		{"too short", "ab", false},
		{"too long", strings.Repeat("a", 33), false},
		{"leading dot", ".admin", false},
		{"leading dash", "-admin", false},
		{"space", "my admin", false},
		{"slash", "a/b/c", false},
		{"path traversal", "../etc", false},
		{"markup", "<b>admin</b>", false},
		{"shell chars", "adm;rm", false},
		{"non-ascii", "adminé", false},
	}
	for _, c := range cases {
		err := ValidateUsername(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s: ValidateUsername(%q) error = %v, want ok=%v", c.name, c.in, err, c.ok)
		}
	}
}

func TestValidateNewPassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		confirm  string
		ok       bool
	}{
		{"valid", "correct-horse", "correct-horse", true},
		{"minimum length", "12345678", "12345678", true},
		{"maximum length", strings.Repeat("a", 72), strings.Repeat("a", 72), true},
		{"empty", "", "", false},
		{"too short", "1234567", "1234567", false},
		{"too long for bcrypt", strings.Repeat("a", 73), strings.Repeat("a", 73), false},
		{"mismatch", "correct-horse", "correct-horsf", false},
		{"missing confirmation", "correct-horse", "", false},
		{"counts characters not bytes", "пароль12", "пароль12", true}, // 8 runes, 14 bytes
	}
	for _, c := range cases {
		err := ValidateNewPassword(c.password, c.confirm)
		if (err == nil) != c.ok {
			t.Errorf("%s: ValidateNewPassword error = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
