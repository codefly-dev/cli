package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// The interface build a solution vendored, and the module pin the
	// workspace deploys: the archive lags the pin.
	vendoredCommit = "2f7c989db7c4ca6dea829f3047035b7eda7bc755"
	moduleBuildPin = "8e79aff27c535a483663d1b3f336574ce9e43326"
	chatArchive    = "acme-assistant-chat-0.3.0-2f7c989d.tgz"
)

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// vendoringWorkspace composes a module `assistant` pinned at pin from
// acme/module-assistant, and a module `backend` whose page bundles the
// assistant's chat package as an archive built at commit. Older builds kept in
// the provenance as history are never selected.
func vendoringWorkspace(t *testing.T, pin, commit, recordedDigest string) string {
	t.Helper()
	archive := "archive bytes"
	if recordedDigest == "" {
		recordedDigest = digestOf(archive)
	}
	workspace := `name: demo
layout: modules
modules:
    - name: backend
    - name: assistant
      source: acme/module-assistant
      module: modules/assistant
      version: "` + pin + `"
      path: ../assistant-checkout
`
	files := map[string]string{
		"workspace.codefly.yaml":                            workspace,
		"modules/backend/module.codefly.yaml":               testModuleYAML("api"),
		"modules/backend/services/api/service.codefly.yaml": testServiceYAML("api"),
		"modules/backend/services/page/package.json": `{"name":"page","dependencies":{
  "@acme/assistant-chat":"file:vendor/` + chatArchive + `",
  "react":"^19.0.0"}}`,
		"modules/backend/playground/.codefly/run/source/package.json": "unreadable generated copy",
		"modules/backend/services/page/vendor/" + chatArchive:         archive,
		"modules/backend/services/page/vendor/acme-old-build.tgz":     "older bytes",
		"modules/backend/services/page/node_modules/x/package.json":   `{"dependencies":{"y":"file:../../nowhere.tgz"}}`,
		"modules/backend/services/page/vendor/assistant-chat.provenance.json": `{"packages":[
  {"repository":"https://github.com/acme/module-assistant","commit":"` + commit + `","package_directory":"modules/assistant/frontend",
   "package":"@acme/assistant-chat@0.3.0","artifact":"` + chatArchive + `","sha256":"` + recordedDigest + `"},
  {"repository":"https://github.com/acme/module-assistant","commit":"0000000000000000000000000000000000000000",
   "package":"@acme/assistant-chat@0.2.0","artifact":"acme-old-build.tgz","sha256":"` + digestOf("older bytes") + `"}]}`,
		"assistant-checkout/module.codefly.yaml": "kind: module\nname: assistant\n",
	}
	root := writeTestWorkspace(t, files)
	return root
}

func TestDoctorWorkspaceRefusesAVendoredPackageBehindItsModulePin(t *testing.T) {
	dir := vendoringWorkspace(t, moduleBuildPin, vendoredCommit, "")
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	diagnostics := findDiagnostics(report, codeVendoredPackageBehindPin)
	if len(diagnostics) != 1 {
		t.Fatalf("want one finding for the selected archive alone, got %d: %s", len(diagnostics), reportJSON(t, report))
	}
	got := diagnostics[0]
	for _, want := range []string{`module "backend"`, chatArchive, "2f7c989db7c4", "8e79aff27c53", `module "assistant"`} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("message %q does not name %q", got.Message, want)
		}
	}
	if got.Status != checkStatusFail || report.Status != readinessStatusNotReady {
		t.Fatalf("a lagging vendored package must fail readiness: %s", reportJSON(t, report))
	}
}

func TestDoctorWorkspaceAcceptsAVendoredPackageAtItsModulePin(t *testing.T) {
	dir := vendoringWorkspace(t, moduleBuildPin, moduleBuildPin, "")
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireNoCode(t, report, codeVendoredPackageBehindPin)
	requireNoCode(t, report, codeVendoredPackageDigest)
}

func TestDoctorWorkspaceRefusesAVendoredArchiveItsProvenanceDoesNotDescribe(t *testing.T) {
	dir := vendoringWorkspace(t, moduleBuildPin, moduleBuildPin, digestOf("other bytes"))
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, codeVendoredPackageDigest, checkStatusFail)
}

// A tag pin is compared with the tag the provenance records; with neither a
// full commit on both sides nor a tag, the two cannot be joined offline and the
// doctor says so instead of passing.
func TestDoctorWorkspaceVendoredPackageWithATagPin(t *testing.T) {
	dir := vendoringWorkspace(t, "v0.1.40", vendoredCommit, "")
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, codeVendoredPackageUncomparable, "warn")
}

func TestRepositoryKeyJoinsAProvenanceURLAndAModuleSource(t *testing.T) {
	for _, spelling := range []string{"https://github.com/Acme/module-assistant.git", "acme/module-assistant", "git@github.com:acme/module-assistant/"} {
		if got := repositoryKey(strings.Replace(spelling, ":", "/", 1)); got != "acme/module-assistant" {
			t.Fatalf("repositoryKey(%q) = %q", spelling, got)
		}
	}
}

func TestDoctorWorkspaceComparesArchiveWithCheckoutSelectedByOverlay(t *testing.T) {
	source, commits := currencySource(t)
	checkout := strings.TrimPrefix(source, "file://")
	root := vendoringWorkspace(t, moduleBuildPin, commits[3], "")
	for path, content := range map[string]string{
		filepath.Join(checkout, "module.codefly.yaml"): "kind: module\nname: assistant\n",
		filepath.Join(root, "codefly.local.yaml"):      "resolve:\n  assistant:\n    path: " + checkout + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report := runReadiness(t, workspaceReadinessOptions{dir: root, module: "backend"})
	requireNoCode(t, report, codeVendoredPackageBehindPin)
	requireNoCode(t, report, codeVendoredPackageUncomparable)
}
