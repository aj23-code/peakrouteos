package backup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const pass = "correct horse battery"

func TestASealedFileOpensWithThePassphraseAndOnlyWithIt(t *testing.T) {
	plain := []byte(strings.Repeat("secret settings ", 1000))
	sealed, err := Seal(plain, pass)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret settings")) || !bytes.HasPrefix(sealed, []byte(magic)) || len(sealed) != headerLen+len(plain)+16 {
		t.Fatalf("%d bytes, plain text visible: %v", len(sealed), bytes.Contains(sealed, []byte("secret")))
	}
	got, err := Open(sealed, pass)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("%v", err)
	}
	for _, wrong := range []string{"", "correct horse battery ", "Correct horse battery", "x"} {
		if _, err := Open(sealed, wrong); !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("%q: %v", wrong, err)
		}
	}
	// two seals of the same thing are not alike (new salt and nonce), and both open
	again, _ := Seal(plain, pass)
	if bytes.Equal(sealed, again) {
		t.Fatal("two files alike")
	}
	if got, err := Open(again, pass); err != nil || !bytes.Equal(got, plain) {
		t.Fatal(err)
	}
}

func TestAnyChangedByteStopsTheFileFromOpening(t *testing.T) {
	sealed, _ := Seal([]byte("settings and keys"), pass)
	// every byte of the layout is covered by the seal: all of the fixed part at the front (fast to refuse) and a sample of the rest
	// (each of those costs a key derivation)
	positions := []int{}
	for i := 0; i < 12; i++ {
		positions = append(positions, i)
	}
	positions = append(positions, 12, 27, 28, 51, headerLen, headerLen+8, len(sealed)-17, len(sealed)-16, len(sealed)-1)
	for _, i := range positions {
		bad := append([]byte{}, sealed...)
		bad[i] ^= 0x01
		_, err := Open(bad, pass)
		if err == nil {
			t.Fatalf("a flipped bit in byte %d was accepted", i)
		}
	}
	// cut off, or longer
	for _, n := range []int{0, 7, headerLen, headerLen + 10, len(sealed) - 1} {
		if _, err := Open(sealed[:n], pass); err == nil {
			t.Errorf("a file cut to %d bytes was accepted", n)
		}
	}
	if _, err := Open(append(append([]byte{}, sealed...), 0), pass); err == nil {
		t.Error("a longer file was accepted")
	}
}

func TestWhatIsNotABackupOrTooNew(t *testing.T) {
	sealed, _ := Seal([]byte("x"), pass)
	if _, err := Open([]byte("hello world, this is not a backup file at all, no way"), pass); !errors.Is(err, ErrNotABackup) {
		t.Fatalf("%v", err)
	}
	newer := append([]byte{}, sealed...)
	newer[8] = FormatVersion + 1
	if _, err := Open(newer, pass); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("%v", err)
	}
	older := append([]byte{}, sealed...)
	older[8] = 0
	if _, err := Open(older, pass); err == nil || !strings.Contains(err.Error(), "old file format") {
		t.Fatalf("%v", err)
	}
	// a hostile file cannot ask for a cost that would use up the router's memory
	for _, params := range [][3]byte{{30, 8, 1}, {16, 255, 1}, {16, 8, 200}, {4, 8, 1}, {20, 16, 1}} {
		hostile := append([]byte{}, sealed...)
		hostile[9], hostile[10], hostile[11] = params[0], params[1], params[2]
		if _, err := Open(hostile, pass); !errors.Is(err, ErrNotABackup) {
			t.Errorf("%v: %v", params, err)
		}
	}
}

func TestPassphraseRules(t *testing.T) {
	for _, bad := range []string{"", "short", "123456789", strings.Repeat("x", MaxPassphrase+1)} {
		if _, err := Seal([]byte("x"), bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
		if CheckPassphrase(bad) == nil {
			t.Errorf("%q passes the check", bad)
		}
	}
	for _, good := range []string{"0123456789", strings.Repeat("x", MaxPassphrase), "pässwörd with ünicode"} {
		if CheckPassphrase(good) != nil {
			t.Errorf("%q refused", good)
		}
	}
}
