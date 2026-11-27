package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Release signing (docs/updates.md, "Signed releases"). Every update file in a GitHub release has a second file next to it, the same
// name ending in ".sig": an Ed25519 signature, written in base64, of the file's SHA-256 checksum and its name. The private key is only
// in the maker's release workflow; the public keys that may sign are in this program (releaseKeys). A checksum alone only shows that a
// download is whole; a signature shows that it came from the one who holds the key, so a replaced file, release or account is not enough
// to put software on a router.

// signedPrefix names what is signed and its version, so a signature made for something else is never taken for one of these.
const signedPrefix = "peakroute-update-v1\n"

// releaseKeys are the public keys (base64, 32 bytes) whose signatures are accepted. More than one is allowed so that a key can be
// replaced: a release signed with the old key tells the routers about the new one by carrying it in this list, and only then is the old
// one taken off.
var releaseKeys = []string{
	"LobmB6qo/oJ0xmmkkfJ8t/y9sPX1nuK1vOmbEbbDH/g=",
}

// ReleaseKeys returns the built-in public keys.
func ReleaseKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, k := range releaseKeys {
		if pk, err := ParsePublicKey(k); err == nil {
			out = append(out, pk)
		}
	}
	return out
}

// ParsePublicKey reads a public key written in base64.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not a public key")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey reads a private key written in base64 (its 32-byte seed).
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("not a private key")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// GenerateKey makes a new key pair, both halves written in base64.
func GenerateKey(rand io.Reader) (public, private string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

func signedMessage(assetName, sum string) []byte {
	return []byte(signedPrefix + strings.ToLower(sum) + "\n" + assetName + "\n")
}

// Sign signs the checksum (hex SHA-256) of the update file called assetName; the result is the text of the ".sig" file.
func Sign(priv ed25519.PrivateKey, assetName, sum string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedMessage(assetName, sum))) + "\n"
}

// SumReader returns the hex SHA-256 of everything r gives.
func SumReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifySignature says whether sig (the text of a ".sig" file) is a signature by one of keys of the update file assetName whose
// checksum is sum. Nothing is accepted when there are no keys.
func VerifySignature(keys []ed25519.PublicKey, assetName, sum, sig string) error {
	if len(keys) == 0 {
		return errors.New("this version has no release key to check a download against")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("the signature of the release cannot be read")
	}
	msg := signedMessage(assetName, sum)
	for _, k := range keys {
		if ed25519.Verify(k, msg, raw) {
			return nil
		}
	}
	return fmt.Errorf("the signature of the release does not match: the file was not signed by the maker of this software")
}
