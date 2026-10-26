package secretgen

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"regexp"
	"testing"
)

func TestGenerateIdentifierIsAnIdentifier(t *testing.T) {
	value, err := Generate(FormatIdentifier, 12)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z][0-9a-f]{24}$`).MatchString(value) {
		t.Errorf("identifier %q is not a letter followed by 24 hex digits", value)
	}
}

func TestParseFormat(t *testing.T) {
	for _, raw := range []string{"hex", "BASE64", " identifier ", "ed25519-pkcs8"} {
		if _, err := ParseFormat(raw); err != nil {
			t.Errorf("ParseFormat(%q) = %v", raw, err)
		}
	}
	if _, err := ParseFormat("uuid"); err == nil {
		t.Error("ParseFormat accepted uuid")
	}
}

// TestGenerateEd25519PKCS8IsAFreshCanonicalPrivateKey: the value is standard
// base64 that round-trips byte for byte, of PKCS#8 DER that parses as an
// Ed25519 private key, and two draws differ.
func TestGenerateEd25519PKCS8IsAFreshCanonicalPrivateKey(t *testing.T) {
	first, err := Generate(FormatEd25519PKCS8, Ed25519SeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.Strict().DecodeString(first)
	if err != nil || base64.StdEncoding.EncodeToString(der) != first {
		t.Fatalf("not canonical standard base64: %v", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatalf("not PKCS#8: %v", err)
	}
	if _, ok := parsed.(ed25519.PrivateKey); !ok {
		t.Fatalf("PKCS#8 holds %T, want an Ed25519 private key", parsed)
	}
	second, err := Generate(FormatEd25519PKCS8, Ed25519SeedBytes)
	if err != nil || second == first {
		t.Fatalf("two draws are the same key or the second failed: %v", err)
	}
}

func TestGenerateEd25519PKCS8RefusesAnotherSize(t *testing.T) {
	for _, size := range []int{16, 64} {
		if _, err := Generate(FormatEd25519PKCS8, size); err == nil {
			t.Errorf("an Ed25519 key was sized %d", size)
		}
	}
}
