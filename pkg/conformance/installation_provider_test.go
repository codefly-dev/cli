package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheCompositionDeliveryIsWiredToReadInstallationDeclarations is a TEXTUAL
// omission detector for one wiring site, and nothing more.
//
// A module's installation declaration reaches the signed authority document
// only if the composition delivery is constructed with a provider that reads
// it. That provider is supplied where the delivery is built, which is a
// different file from the one the delivery's own tests exercise — those
// construct the provider themselves, so dropping it at the construction site
// is invisible to them, and the symptom is a delivery that silently carries no
// declaration while the Core manifest carries one. Authority then refuses the
// pair, so the failure is loud at install and far from this line.
//
// Comments are stripped before matching, so a comment naming the reader does
// not satisfy this. It is a cheap backstop against one omission, not proof the
// wiring is correct.
func TestTheCompositionDeliveryIsWiredToReadInstallationDeclarations(t *testing.T) {
	root := repositoryRoot(t)
	var built, reads []string
	err := filepath.WalkDir(filepath.Join(root, "cmd"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		body := codeOf(t, path, readFile(t, path))
		if !strings.Contains(body, "NewCompositionDelivery(") {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		relative = filepath.ToSlash(relative)
		built = append(built, relative)
		if strings.Contains(body, "InstallationDeclaration(") {
			reads = append(reads, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(built) == 0 {
		t.Fatal("no command file constructs a composition delivery; this guard has lost its subject")
	}
	for _, file := range built {
		found := false
		for _, reader := range reads {
			found = found || reader == file
		}
		if !found {
			t.Errorf("%s constructs a composition delivery without reading installation declarations: pass gitops.InstallationDeclaration as its installation provider, or the signed authority document carries no selector or edges while the Core manifest carries both, and the pair is refused at install", file)
		}
	}
}
