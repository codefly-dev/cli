// Package secretgen is the one generator of random secret values: `codefly
// config generate` writes what it returns into a local configuration file, and
// `codefly deploy secrets` into an environment's secret store, so a value is
// minted the same way wherever it lands.
package secretgen

import (
	"crypto/rand"
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
)

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
	default:
		return "", fmt.Errorf("unknown format %q: expected hex, base64 or identifier", raw)
	}
}

// Generate returns a cryptographically random value of size bytes in the given
// encoding.
func Generate(format Format, size int) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("--bytes must be positive, got %d", size)
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
