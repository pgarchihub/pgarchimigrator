// Command pgarchisign implements AOL-STD-DEPLOY-001 §5.1's signed
// artifact descriptor — a small, SEPARATE binary from pgarchimigrator
// itself (never shipped inside a release package, never imported by
// any runtime package) because signing is a release-process concern,
// not something the product needs at runtime; keeping it separate means
// the main binary's dependency graph and attack surface don't grow to
// accommodate a capability ordinary users never invoke.
//
// Three subcommands:
//
//	pgarchisign genkey --out-dir <dir>
//	  Generates a NEW Ed25519 key pair for development/testing. This is
//	  NOT the real "ArchiOrbit Labs" release key — that key's
//	  generation and custody (HSM, CI secret store, who holds access)
//	  is an organizational decision this tool cannot make or fabricate
//	  on the organization's behalf. Use this subcommand to produce a
//	  throwaway key pair for local testing of the sign/verify flow
//	  itself.
//
//	pgarchisign sign --artifact <path> --key <private-key-path> \
//	    --key-id <publisherKeyId> --product-id <id> --version <semver>
//	  Computes the artifact's SHA-256, builds the descriptor, signs it,
//	  and writes <artifact>.descriptor.json alongside the artifact.
//
//	pgarchisign verify --descriptor <path> --pubkey <public-key-path> \
//	    [--artifact <path>]
//	  Verifies a descriptor's signature against a public key, and
//	  (if --artifact is given) additionally recomputes the artifact's
//	  own SHA-256 and confirms it matches the descriptor's sha256 field
//	  — AOL-STD-DEPLOY-001 §5.1: "Revoked or unknown keys, mutated
//	  bytes and digest mismatches MUST be rejected before execution."
//	  This subcommand is what a certified installer adapter (or CI's
//	  own "clean-host install tests" stage, §13) is expected to run
//	  before trusting a package at all.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// descriptor mirrors AOL-STD-DEPLOY-001 §5.1's exact JSON shape.
type descriptor struct {
	ArtifactID     string `json:"artifactId"`
	ProductID      string `json:"productId"`
	Version        string `json:"version"`
	SHA256         string `json:"sha256"`
	PublisherKeyID string `json:"publisherKeyId"`
	Signature      string `json:"signature"`
}

// signingBytes returns the exact, deterministic byte sequence that gets
// signed — every descriptor field the standard says the descriptor
// "MUST bind" (§5.1), in a fixed order, delimited unambiguously.
// Deliberately NOT "marshal the struct to JSON and sign that": relying
// on encoding/json's field ordering (which IS stable in Go today, but
// is an implementation detail the encoding/json docs don't guarantee as
// a permanent contract) as a cryptographic signing input would make
// this tool's own correctness depend on something outside its control.
// This function is the one place both sign and verify compute the
// signed bytes, so they can never drift apart from each other.
func signingBytes(d descriptor) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%s", d.ArtifactID, d.ProductID, d.Version, d.SHA256, d.PublisherKeyID))
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "genkey":
		err = runGenkey(os.Args[2:])
	case "sign":
		err = runSign(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: pgarchisign <genkey|sign|verify> [flags]")
	os.Exit(1)
}

func runGenkey(args []string) error {
	fs := flag.NewFlagSet("genkey", flag.ExitOnError)
	outDir := fs.String("out-dir", ".", "directory to write ed25519-private.key and ed25519-public.key into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate key pair: %w", err)
	}

	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	privPath := filepath.Join(*outDir, "ed25519-private.key")
	pubPath := filepath.Join(*outDir, "ed25519-public.key")
	if err := os.WriteFile(privPath, []byte(hex.EncodeToString(priv)), 0o600); err != nil {
		return fmt.Errorf("failed to write private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(hex.EncodeToString(pub)), 0o644); err != nil {
		return fmt.Errorf("failed to write public key: %w", err)
	}

	fmt.Println("Generated a NEW development/test Ed25519 key pair — this is NOT the real ArchiOrbit Labs release key.")
	fmt.Println("Private key:", privPath, "(0600, keep secret)")
	fmt.Println("Public key: ", pubPath)
	return nil
}

func runSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	artifactPath := fs.String("artifact", "", "path to the artifact to sign (required)")
	keyPath := fs.String("key", "", "path to a hex-encoded Ed25519 private key (required)")
	keyID := fs.String("key-id", "", "publisherKeyId, e.g. archiorbitlabs-release-2026 (required)")
	productID := fs.String("product-id", "pgarchimigrator", "productId")
	version := fs.String("version", "", "semantic version, e.g. 2.0.0 (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *artifactPath == "" || *keyPath == "" || *keyID == "" || *version == "" {
		return fmt.Errorf("--artifact, --key, --key-id, and --version are all required")
	}

	priv, err := readPrivateKey(*keyPath)
	if err != nil {
		return err
	}

	sum, err := sha256File(*artifactPath)
	if err != nil {
		return fmt.Errorf("failed to hash artifact: %w", err)
	}

	d := descriptor{
		ArtifactID:     filepath.Base(*artifactPath),
		ProductID:      *productID,
		Version:        *version,
		SHA256:         sum,
		PublisherKeyID: *keyID,
	}
	sig := ed25519.Sign(priv, signingBytes(d))
	d.Signature = hex.EncodeToString(sig)

	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	outPath := *artifactPath + ".descriptor.json"
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		return fmt.Errorf("failed to write descriptor: %w", err)
	}
	fmt.Println("Descriptor written:", outPath)
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	descriptorPath := fs.String("descriptor", "", "path to the descriptor JSON to verify (required)")
	pubKeyPath := fs.String("pubkey", "", "path to a hex-encoded Ed25519 public key (required)")
	artifactPath := fs.String("artifact", "", "optional: also recompute and compare the artifact's own SHA-256")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *descriptorPath == "" || *pubKeyPath == "" {
		return fmt.Errorf("--descriptor and --pubkey are both required")
	}

	raw, err := os.ReadFile(*descriptorPath)
	if err != nil {
		return fmt.Errorf("failed to read descriptor: %w", err)
	}
	var d descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("failed to parse descriptor: %w", err)
	}

	pub, err := readPublicKey(*pubKeyPath)
	if err != nil {
		return err
	}

	sig, err := hex.DecodeString(d.Signature)
	if err != nil {
		return fmt.Errorf("descriptor signature is not valid hex: %w", err)
	}
	if !ed25519.Verify(pub, signingBytes(d), sig) {
		return fmt.Errorf("SIGNATURE INVALID — descriptor does not match this public key, or was tampered with")
	}
	fmt.Println("Signature OK.")

	if *artifactPath != "" {
		sum, err := sha256File(*artifactPath)
		if err != nil {
			return fmt.Errorf("failed to hash artifact: %w", err)
		}
		if sum != d.SHA256 {
			return fmt.Errorf("DIGEST MISMATCH — artifact sha256 %s does not match descriptor sha256 %s", sum, d.SHA256)
		}
		fmt.Println("Artifact digest OK.")
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}
	decoded, err := hex.DecodeString(trimNewline(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("private key is not valid hex: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key has unexpected length %d (expected %d)", len(decoded), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(decoded), nil
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read public key: %w", err)
	}
	decoded, err := hex.DecodeString(trimNewline(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("public key is not valid hex: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key has unexpected length %d (expected %d)", len(decoded), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
