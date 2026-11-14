package imagedigest

import (
	"strings"
	"testing"
)

func TestFromRepoDigests(t *testing.T) {
	a, b := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	if got, err := fromRepoDigests("img", []string{"ghcr.io/x/img@" + a, "mirror.io/x/img@" + a}); err != nil || got != a {
		t.Fatalf("one digest under two names = %q, %v; want %q", got, err, a)
	}
	for name, references := range map[string][]string{
		"none":      nil,
		"malformed": {"ghcr.io/x/img@sha256:short", "ghcr.io/x/img"},
		"ambiguous": {"ghcr.io/x/img@" + a, "ghcr.io/x/img@" + b},
	} {
		if got, err := fromRepoDigests("img", references); err == nil {
			t.Errorf("%s: got %q, want an error", name, got)
		}
	}
}
