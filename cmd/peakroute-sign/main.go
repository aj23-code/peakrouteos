// Command peakroute-sign makes and checks the signatures of release files (internal/update/signing.go). The release workflows use it;
// the private key is a repository secret and never in the source.
//
//	peakroute-sign keygen                          writes a new key pair to the terminal: the public key goes into internal/update/signing.go
//	peakroute-sign sign [-keyenv NAME] FILE...     writes FILE.sig next to each file (the private key is in the variable NAME, default RELEASE_SIGNING_KEY)
//	peakroute-sign verify [-pub KEY] FILE...       checks each FILE against FILE.sig (with the keys built into the software unless -pub is given:
//	                                               this is what the release workflow runs, so a release signed with the wrong key is not published)
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"peakroute/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "peakroute-sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: peakroute-sign keygen | sign [-keyenv NAME] FILE... | verify [-pub KEY] FILE...")
	os.Exit(2)
}

func keygen() error {
	pub, priv, err := update.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Printf("public:  %s\nprivate: %s\n", pub, priv)
	return nil
}

func sumOf(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return update.SumReader(f)
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	env := fs.String("keyenv", "RELEASE_SIGNING_KEY", "the variable that holds the private key")
	fs.Parse(args)
	priv, err := update.ParsePrivateKey(os.Getenv(*env))
	if err != nil {
		return fmt.Errorf("the variable %s does not hold a private key", *env)
	}
	if fs.NArg() == 0 {
		usage()
	}
	for _, path := range fs.Args() {
		sum, err := sumOf(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".sig", []byte(update.Sign(priv, filepath.Base(path), sum)), 0o644); err != nil {
			return err
		}
		fmt.Println("signed", path)
	}
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	pubS := fs.String("pub", "", "the public key (base64); the keys built into this software when empty")
	fs.Parse(args)
	keys := update.ReleaseKeys()
	if *pubS != "" {
		pub, err := update.ParsePublicKey(*pubS)
		if err != nil {
			return err
		}
		keys = []ed25519.PublicKey{pub}
	}
	if fs.NArg() == 0 {
		usage()
	}
	for _, path := range fs.Args() {
		sum, err := sumOf(path)
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(path + ".sig")
		if err != nil {
			return err
		}
		if err := update.VerifySignature(keys, filepath.Base(path), sum, string(sig)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Println("ok", path)
	}
	return nil
}
