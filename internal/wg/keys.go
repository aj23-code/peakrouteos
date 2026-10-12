// Package wg is the small part of WireGuard the router does itself: making and checking keys, and reading what `wg` says about a
// running tunnel. The tunnels themselves are made by wg-quick from the files the translator writes. docs/vpn.md describes it.
package wg

import (
	"crypto/rand"
	"encoding/base64"
	"errors"

	"golang.org/x/crypto/curve25519"
)

const keyLen = 32

// ValidKey reports whether s is a WireGuard key: 32 bytes in standard base64 (44 characters, ending in "=").
func ValidKey(s string) bool {
	if len(s) != 44 || s[43] != '=' {
		return false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == keyLen
}

// GeneratePrivateKey makes a new private key the way `wg genkey` does: 32 random bytes with the bits Curve25519 asks for fixed.
func GeneratePrivateKey() (string, error) {
	var k [keyLen]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

// PublicKey derives the public key of a private key (as `wg pubkey` does).
func PublicKey(private string) (string, error) {
	if !ValidKey(private) {
		return "", errors.New("not a WireGuard key")
	}
	b, _ := base64.StdEncoding.DecodeString(private)
	pub, err := curve25519.X25519(b, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// GenerateKeyPair makes a private key and its public key.
func GenerateKeyPair() (private, public string, err error) {
	private, err = GeneratePrivateKey()
	if err != nil {
		return "", "", err
	}
	public, err = PublicKey(private)
	return private, public, err
}

// GeneratePresharedKey makes the extra shared secret a pair of peers may add (32 random bytes, as `wg genpsk` does).
func GeneratePresharedKey() (string, error) {
	var k [keyLen]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}
