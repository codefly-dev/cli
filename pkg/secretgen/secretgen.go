// Package secretgen is the one generator of random secret values: `codefly
// config generate` writes what it returns into a local configuration file, and
// `codefly deploy secrets` into an environment's secret store, so a value is
// minted the same way wherever it lands.
package secretgen

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Format selects the encoding of a generated secret.
type Format string

const (
	FormatHex    Format = "hex"
	FormatBase64 Format = "base64"
	// FormatIdentifier is a lowercase letter followed by hex: a value that must
	// also be a SQL or DNS identifier, such as a database owner's name.
	FormatIdentifier Format = "identifier"
	// FormatEd25519PKCS8 is a fresh Ed25519 PRIVATE KEY: canonical standard
	// base64 of its PKCS#8 DER encoding. A service that signs with a key it must
	// keep for its whole life (a witness whose chain is verified under one key)
	// needs the key minted once, in the store, never typed and never generated
	// at boot. Its size is the algorithm's (a 32-byte seed); it takes no `bytes`.
	FormatEd25519PKCS8 Format = "ed25519-pkcs8"
)

// Ed25519SeedBytes is the only size FormatEd25519PKCS8 accepts: the size of
// an Ed25519 seed, which is also what every generator defaults to.
const Ed25519SeedBytes = ed25519.SeedSize

// identifierLead makes an identifier start with a letter, as SQL and DNS
// identifiers must; hex alone may start with a digit.
const identifierLead = "u"

// ParseFormat validates a format name.
func ParseFormat(raw string) (Format, error) {
	switch Format(strings.TrimSpace(strings.ToLower(raw))) {
	case FormatHex:
		return FormatHex, nil
	case FormatBase64:
		return FormatBase64, nil
	case FormatIdentifier:
		return FormatIdentifier, nil
	case FormatEd25519PKCS8:
		return FormatEd25519PKCS8, nil
	default:
		return "", fmt.Errorf("unknown format %q: expected hex, base64, identifier or ed25519-pkcs8", raw)
	}
}

// Generate returns a cryptographically random value of size bytes in the given
// encoding.
func Generate(format Format, size int) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("--bytes must be positive, got %d", size)
	}
	if format == FormatEd25519PKCS8 {
		return generateEd25519PKCS8(size)
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate %d random bytes: %w", size, err)
	}
	switch format {
	case FormatHex:
		return hex.EncodeToString(buf), nil
	case FormatBase64:
		return base64.StdEncoding.EncodeToString(buf), nil
	case FormatIdentifier:
		return identifierLead + hex.EncodeToString(buf), nil
	default:
		return "", fmt.Errorf("unknown format %q", format)
	}
}

// generateEd25519PKCS8 mints an Ed25519 private key from the system's
// cryptographic randomness and encodes it as canonical standard base64 of its
// PKCS#8 DER. A size other than the seed's is refused rather than ignored: a
// declaration asking for a different key size asked for something this format
// cannot mean.
func generateEd25519PKCS8(size int) (string, error) {
	if size != Ed25519SeedBytes {
		return "", fmt.Errorf("format %s is a %d-byte Ed25519 seed; bytes %d cannot size it", FormatEd25519PKCS8, Ed25519SeedBytes, size)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate an Ed25519 key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", fmt.Errorf("encode the Ed25519 key as PKCS#8: %w", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}
