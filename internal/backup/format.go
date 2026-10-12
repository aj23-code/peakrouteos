// Package backup makes and restores the router's backups (docs/backup.md).
//
// A backup is one file, always encrypted with a passphrase (it holds every private key): a small header in the clear, then the
// archive sealed with XChaCha20-Poly1305 under a key derived from the passphrase with scrypt. The header is part of what is
// authenticated, so a changed byte anywhere fails to open. Nothing here knows what is inside; bundle.go does.
package backup

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"
)

const (
	magic = "PRBACKUP"
	// FormatVersion is the version of the file layout (not of what is inside).
	FormatVersion = 1

	scryptLogN = 16 // 64 MiB and about a quarter of a second: for a backup, not a login
	scryptR    = 8
	scryptP    = 1

	saltLen = 16

	// MinPassphrase and MaxPassphrase are the lengths (in bytes) a passphrase may have.
	MinPassphrase = 10
	MaxPassphrase = 256

	// MaxFile is the largest backup file the router will open.
	MaxFile = 256 << 20
)

// header: magic (8), version (1), log2 N (1), r (1), p (1), salt (16), nonce (24) = 52 bytes, then the sealed archive
const headerLen = 8 + 4 + saltLen + chacha20poly1305.NonceSizeX

// ErrWrongPassphrase is what opening gives for a wrong passphrase and for a damaged file alike: the two cannot be told apart.
var ErrWrongPassphrase = errors.New("the passphrase is wrong, or the file is damaged")

// ErrNotABackup is for a file that is not one of ours.
var ErrNotABackup = errors.New("that is not a PeakRoute OS backup file")

// CheckPassphrase says whether a passphrase is acceptable for a new backup.
func CheckPassphrase(p string) error {
	switch {
	case len(p) < MinPassphrase:
		return fmt.Errorf("the passphrase must have at least %d characters", MinPassphrase)
	case len(p) > MaxPassphrase:
		return fmt.Errorf("the passphrase must have at most %d characters", MaxPassphrase)
	}
	return nil
}

func deriveKey(pass string, salt []byte, logN, r, p int) ([]byte, error) {
	return scrypt.Key([]byte(pass), salt, 1<<uint(logN), r, p, chacha20poly1305.KeySize)
}

// Seal encrypts plain with the passphrase.
func Seal(plain []byte, pass string) ([]byte, error) {
	if err := CheckPassphrase(pass); err != nil {
		return nil, err
	}
	h := make([]byte, headerLen)
	copy(h, magic)
	h[8], h[9], h[10], h[11] = FormatVersion, scryptLogN, scryptR, scryptP
	if _, err := rand.Read(h[12:headerLen]); err != nil {
		return nil, err
	}
	key, err := deriveKey(pass, h[12:12+saltLen], scryptLogN, scryptR, scryptP)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := h[12+saltLen : headerLen]
	return append(h, aead.Seal(nil, nonce, plain, h[:12+saltLen])...), nil
}

// Open decrypts a backup file. Nothing in it is trusted before the seal has checked out.
func Open(data []byte, pass string) ([]byte, error) {
	if len(data) < headerLen+chacha20poly1305.Overhead || string(data[:8]) != magic {
		return nil, ErrNotABackup
	}
	if v := data[8]; v != FormatVersion {
		if v > FormatVersion {
			return nil, fmt.Errorf("this backup was made by a newer version of PeakRoute OS (file format %d); update this router first", v)
		}
		return nil, fmt.Errorf("this backup has an old file format (%d) that is not understood", v)
	}
	logN, r, p := int(data[9]), int(data[10]), int(data[11])
	// the cost is in the file, so a hostile file must not be able to ask for the moon
	if logN < 12 || logN > 20 || r < 1 || r > 16 || p < 1 || p > 4 || (1<<uint(logN))*r > 1<<20 {
		return nil, ErrNotABackup
	}
	key, err := deriveKey(pass, data[12:12+saltLen], logN, r, p)
	if err != nil {
		return nil, ErrNotABackup
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, data[12+saltLen:headerLen], data[headerLen:], data[:12+saltLen])
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	return plain, nil
}
