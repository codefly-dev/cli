package generators

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
)

// Metadata recovery must require an explicit single-contract force and must
// still stop before writing when the language package cannot be generated.
func TestConflictedLibraryManifestRecoveryPreservesPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		force          bool
		entries        int
		want           string
	}{
		{"conflict refused", "<<<<<<< HEAD\nversion: 0.1.0\n=======\nversion: 0.2.0\n>>>>>>> branch\n", false, 1, "cannot parse existing"},
		{"forced conflict reaches package guard", "<<<<<<< HEAD\nversion: 0.1.0\n=======\nversion: 0.2.0\n>>>>>>> branch\n", true, 1, "cannot set the typescript client's version"},
		{"forced invalid language reaches package guard", "sources: []\nlanguages: invalid\n", true, 1, "cannot set the typescript client's version"},
		{"aggregate cannot infer its version", "version: [\n", true, 2, "cannot parse existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(output, resources.LibraryConfigurationName), []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			langDir := filepath.Join(output, "typescript")
			if err := os.MkdirAll(langDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(langDir, "package.json"), []byte(`{"name":"@acme/example-client","private":true}`), 0o644); err != nil {
				t.Fatal(err)
			}
			entries := []ContractEntry{{
				Endpoint:       composition.APIContractEndpoint{Service: "api", Endpoint: "connect", Kind: composition.APIContractKindProtobuf, Package: "example.v1"},
				PackageVersion: "0.2.0", ModuleName: "example",
			}}
			if tc.entries == 2 {
				entries = append(entries, ContractEntry{
					Endpoint:       composition.APIContractEndpoint{Service: "other", Endpoint: "connect", Kind: composition.APIContractKindProtobuf, Package: "other.v1"},
					PackageVersion: "0.3.0", ModuleName: "other",
				})
			}
			before := snapshotTree(t, output)
			err := GenerateClientLibrary(context.Background(), &ClientLibraryRequest{
				Name: "example-client", Output: output, Force: tc.force,
				Languages: []languages.Language{languages.TYPESCRIPT}, Entries: entries,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want refusal containing %q", err, tc.want)
			}
			if after := snapshotTree(t, output); after != before {
				t.Fatalf("a refused generation changed the library:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestForcedLibraryRecoveryDoesNotIgnoreFilesystemErrors(t *testing.T) {
	output := t.TempDir()
	if err := os.Mkdir(filepath.Join(output, resources.LibraryConfigurationName), 0o755); err != nil {
		t.Fatal(err)
	}
	err := GenerateClientLibrary(context.Background(), &ClientLibraryRequest{
		Name: "example-client", Output: output, Force: true,
		Languages: []languages.Language{languages.TYPESCRIPT},
		Entries: []ContractEntry{{
			Endpoint:       composition.APIContractEndpoint{Service: "api", Endpoint: "connect", Kind: composition.APIContractKindProtobuf},
			PackageVersion: "0.2.0", ModuleName: "example",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("got %v, want the filesystem error", err)
	}
}
