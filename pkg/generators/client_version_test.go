package generators

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/languages"
)

// A generated client carries the library's version, the producer's own, not
// the scaffold's 0.0.1: the document store's library said 0.1.14 while its
// TypeScript client's package.json said 0.0.1 and was published as that.
func TestTypeScriptClientCarriesTheLibraryVersion(t *testing.T) {
	langDir := t.TempDir()
	if err := writeTypeScriptScaffold(langDir, "@acme/billing-client"); err != nil {
		t.Fatal(err)
	}
	if err := stampPackageVersion(languages.TYPESCRIPT, langDir, "0.1.14"); err != nil {
		t.Fatal(err)
	}
	if got := packageJSON(t, langDir)["version"]; got != "0.1.14" {
		t.Fatalf("package.json version = %v, want 0.1.14", got)
	}
	// A later generation at a newer producer version moves it again.
	if err := stampPackageVersion(languages.TYPESCRIPT, langDir, "0.1.15"); err != nil {
		t.Fatal(err)
	}
	if got := packageJSON(t, langDir)["version"]; got != "0.1.15" {
		t.Fatalf("package.json version = %v, want 0.1.15", got)
	}
}

// Only the top-level member moves: a hand-edited, reformatted package.json
// keeps every other byte, and a nested "version" key is not the package's.
func TestTypeScriptVersionStampKeepsHandEdits(t *testing.T) {
	langDir := t.TempDir()
	original := `{"engines":{"version":"keep"},"name":"@acme/billing-client","private":true,"version":"0.0.1"}`
	if err := os.WriteFile(filepath.Join(langDir, "package.json"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stampPackageVersion(languages.TYPESCRIPT, langDir, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(langDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(original, `"version":"0.0.1"`, `"version":"2.0.0"`, 1)
	if string(got) != want {
		t.Fatalf("package.json:\ngot  %s\nwant %s", got, want)
	}
}

func TestPythonClientCarriesTheLibraryVersion(t *testing.T) {
	langDir := t.TempDir()
	if err := writePythonScaffold(langDir, "billing_client"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(langDir, "pyproject.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handEdited := string(data) + "\n[tool.pytest.ini_options]\nminversion = \"8.0\"\n"
	if err := os.WriteFile(path, []byte(handEdited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stampPackageVersion(languages.PYTHON, langDir, "0.1.14"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(handEdited, `version = "0.0.1"`, `version = "0.1.14"`, 1)
	if string(got) != want {
		t.Fatalf("pyproject.toml:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A library with no version is refused by name rather than stamped empty.
func TestAClientWithoutAVersionIsRefused(t *testing.T) {
	langDir := t.TempDir()
	if err := writeTypeScriptScaffold(langDir, "@acme/billing-client"); err != nil {
		t.Fatal(err)
	}
	err := stampPackageVersion(languages.TYPESCRIPT, langDir, " ")
	if err == nil || !strings.Contains(err.Error(), "declares no version") {
		t.Fatalf("err = %v, want a refusal naming the missing version", err)
	}
}

// A Go module's version is its tag: nothing is written.
func TestGoClientVersionIsItsTag(t *testing.T) {
	if err := stampPackageVersion(languages.GO, t.TempDir(), "0.1.14"); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryVersionIsTheProducersForOneContract(t *testing.T) {
	entries := []ContractEntry{{PackageVersion: "0.1.14"}}
	if got := libraryVersion(t.TempDir(), entries); got != "0.1.14" {
		t.Fatalf("libraryVersion = %q, want 0.1.14", got)
	}
}

func packageJSON(t *testing.T, langDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(langDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("package.json is not valid JSON: %v\n%s", err, data)
	}
	return doc
}
