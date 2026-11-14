package generators

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// stubCompanionRun replaces what companionUsed reads of the run: whether the
// engine was up, and the digest the daemon holds for the image.
func stubCompanionRun(t *testing.T, engine bool, digest func(context.Context, string) (string, error)) {
	t.Helper()
	previousEngine, previousDigest := dockerEngineRunning, localImageDigest
	dockerEngineRunning = func(context.Context) bool { return engine }
	localImageDigest = digest
	t.Cleanup(func() { dockerEngineRunning, localImageDigest = previousEngine, previousDigest })
}

func recordedCompanion(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries := []ContractEntry{releaseClientEntry("billing", "0.1.0")}
	if err := writeLibraryManifest(t.Context(), dir, "client", "0.1.0", entries, nil, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, resources.LibraryConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		GeneratedBy struct {
			Companion string `yaml:"companion"`
		} `yaml:"generated-by"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.GeneratedBy.Companion
}

// The companion is recorded by the digest of the image that ran, beside its
// tag: a tag the registry repoints cannot say which plugin set generated the
// bytes (codefly-dev/cli#1275).
func TestTheLibraryRecordsTheCompanionThatRanByDigest(t *testing.T) {
	image, err := coreproto.CompanionImage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tag := image.FullName()
	digest := "sha256:" + strings.Repeat("ab", 32)
	var asked string
	stubCompanionRun(t, true, func(_ context.Context, reference string) (string, error) {
		asked = reference
		return digest, nil
	})
	if got, want := recordedCompanion(t), tag+"@"+digest; got != want {
		t.Fatalf("generated-by.companion = %q, want %q", got, want)
	}
	if asked != tag {
		t.Fatalf("the digest was read for %q, not the image that ran (%q)", asked, tag)
	}
}

// With nothing to pin, the record keeps the tag rather than inventing a digest:
// without an engine the host's tools ran, and a locally built image has no
// registry digest.
func TestTheCompanionIsRecordedByTagWhenNoImageDigestExists(t *testing.T) {
	image, err := coreproto.CompanionImage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	read := false
	stubCompanionRun(t, false, func(context.Context, string) (string, error) { read = true; return "", nil })
	if got := recordedCompanion(t); got != image.FullName() || read {
		t.Fatalf("with no engine: companion = %q (digest read: %v), want %q", got, read, image.FullName())
	}
	stubCompanionRun(t, true, func(context.Context, string) (string, error) { return "", errors.New("no registry digest") })
	if got := recordedCompanion(t); got != image.FullName() {
		t.Fatalf("with no digest: companion = %q, want %q", got, image.FullName())
	}
}
