package syslogfwd

import (
	"os"
	"testing"
	"time"
)

func writeExecutable(path, content string) error { return os.WriteFile(path, []byte(content), 0755) }
func pathEnv() string                            { return os.Getenv("PATH") }
func readFile(path string) (string, error)       { b, err := os.ReadFile(path); return string(b), err }

func TestMain(m *testing.M) {
	maxAge = 100 * 365 * 24 * time.Hour // the tests use fixed times; the one about old packets sets its own
	os.Exit(m.Run())
}
