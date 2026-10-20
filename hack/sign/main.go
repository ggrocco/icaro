// Command sign produces and checks the detached ed25519 signature over a
// release's checksums.txt (ADR 0021). The private key is the base64
// 32-byte seed in $ICARO_SIGNING_KEY (a CI secret); it never touches disk.
//
//	go run ./hack/sign keygen                 # prints seed and public key
//	go run ./hack/sign sign -in checksums.txt -out checksums.txt.sig
//	go run ./hack/sign verify -pub key.pem -in checksums.txt -sig checksums.txt.sig
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"icaro/internal/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: sign keygen | sign sign -in FILE -out SIG | sign verify -pub KEY -in FILE -sig SIG")
	}
	switch args[0] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		p, err := update.PublicKeyPEM(pub)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "ICARO_SIGNING_KEY=%s\n", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Fprintf(stdout, "public key (base64): %s\n%s", base64.StdEncoding.EncodeToString(pub), p)
		return nil
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ContinueOnError)
		in := fs.String("in", "", "file to sign")
		out := fs.String("out", "", "signature output (raw 64 bytes)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("ICARO_SIGNING_KEY")))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("ICARO_SIGNING_KEY must be a base64 32-byte ed25519 seed")
		}
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), data)
		return os.WriteFile(*out, sig, 0o644) //nolint:gosec // signatures are public
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		pubPath := fs.String("pub", "", "public key file (PEM or base64)")
		in := fs.String("in", "", "signed file")
		sigPath := fs.String("sig", "", "signature file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		pubRaw, err := os.ReadFile(*pubPath)
		if err != nil {
			return err
		}
		pub, err := update.ParsePublicKey(string(pubRaw))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(*sigPath)
		if err != nil {
			return err
		}
		if err := update.VerifySignature(pub, data, sig); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "signature OK")
		return nil
	}
	return fmt.Errorf("unknown subcommand %q", args[0])
}
