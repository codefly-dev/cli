package secretgen

import (
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
	for _, raw := range []string{"hex", "BASE64", " identifier "} {
		if _, err := ParseFormat(raw); err != nil {
			t.Errorf("ParseFormat(%q) = %v", raw, err)
		}
	}
	if _, err := ParseFormat("uuid"); err == nil {
		t.Error("ParseFormat accepted uuid")
	}
}
