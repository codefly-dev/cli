package prerelease

import "testing"

// TestARepositorysOwnOrganisationIsFirstPartyWithoutAFlag: the CLI names no
// consumer's organisation, so the gate learns it from the go.mod it reads. A
// default that left the consumer's organisation out would pass its gate without
// checking anything.
func TestARepositorysOwnOrganisationIsFirstPartyWithoutAFlag(t *testing.T) {
	content := []byte(`module github.com/acme/module-notes/clients/go

go 1.27

require (
	github.com/acme/module-greeter/clients/go v0.0.0-20260926222912-289b830b06a9
	github.com/other/library v0.0.0-20260926222912-289b830b06a9
)
`)
	findings, err := scanGoMod("go.mod", content, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Key != "require github.com/acme/module-greeter/clients/go" {
		t.Fatalf("want exactly the sibling require flagged, got %+v", findings)
	}
}

func TestOwnerPrefix(t *testing.T) {
	for path, want := range map[string]string{
		"github.com/acme/tools/go": "github.com/acme/",
		"github.com/acme/tools":    "github.com/acme/",
		"github.com/acme":          "",
		"tools/go/x":               "",
	} {
		if got := ownerPrefix(path); got != want {
			t.Errorf("ownerPrefix(%q) = %q, want %q", path, got, want)
		}
	}
}
