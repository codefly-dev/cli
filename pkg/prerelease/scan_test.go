package prerelease

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// findingKeys is the "file:line key=version" set a scan produced, which is what a
// failure report has to be able to name.
func findingKeys(findings []Finding) []string {
	keys := make([]string, 0, len(findings))
	for _, finding := range findings {
		keys = append(keys, finding.Location()+" "+finding.Key+"="+finding.Version)
	}
	sort.Strings(keys)
	return keys
}

func scanFixture(t *testing.T, fixture string, options Options) *Result {
	t.Helper()
	result, err := Scan(filepath.Join("testdata", fixture), options)
	if err != nil {
		t.Fatalf("scan %s: %v", fixture, err)
	}
	return result
}

// TestTaggedModulesWithDevAgentPinsAreRefused is the regression the gate exists
// for: the shape two released module tags actually shipped — five service agent
// pins at dev prereleases, each with the DEV PIN comment that was supposed to be
// enough. A comment is not a gate, which is why both tags were cut anyway.
func TestTaggedModulesWithDevAgentPinsAreRefused(t *testing.T) {
	result := scanFixture(t, "module-with-dev-pins", Options{})
	if result.OK() {
		t.Fatal("the module that shipped dev agent pins passed the gate")
	}
	want := []string{
		"module/services/accounts/service.codefly.yaml:8 agent.version=0.1.48-dev.e87db5e08865",
		"module/services/auth-gateway/service.codefly.yaml:6 agent.version=0.1.48-dev.e87db5e08865",
		"module/services/frontend/service.codefly.yaml:6 agent.version=0.0.159-dev.1ed5001fd8b4",
		"module/services/store/service.codefly.yaml:8 agent.version=0.0.139-dev.9785d12f3700",
	}
	got := findingKeys(result.Blocking())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("blocking findings:\ngot:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// A DEV PIN comment on a service pin buys nothing: only an agent-overrides
	// entry may carry a prerelease, and only on the default branch.
	for _, finding := range result.Blocking() {
		if finding.Carrier != CarrierConfig {
			t.Errorf("%s: carrier %q, want %q", finding.Location(), finding.Carrier, CarrierConfig)
		}
	}
	// The clean redis pin in the same module is not swept up.
	for _, finding := range result.Findings {
		if strings.Contains(finding.File, "cache/") {
			t.Errorf("the released redis pin was flagged: %s", finding.Location())
		}
	}
}

// TestCleanModulesPass is the other half of the same regression: a module release
// verified clean must not fail. It pins released agents and carries first-party Go
// pseudo-versions anyway — three of the four real clean releases did — so this is
// also what pins the go.mod policy. A gate that cannot tell this fixture from the
// one above is not a gate, it is a grep for "-".
func TestCleanModulesPass(t *testing.T) {
	result := scanFixture(t, "clean-module", Options{})
	if !result.OK() {
		t.Fatalf("a clean module failed the gate:\n%v", result.Failure())
	}
	if len(result.Allowed()) == 0 {
		t.Fatal("the fixture no longer carries first-party pseudo-versions, so it cannot pin the go.mod policy")
	}
	for _, finding := range result.Allowed() {
		if finding.Carrier != CarrierGoModule {
			t.Errorf("%s: unexpected allowed carrier %q", finding.Location(), finding.Carrier)
		}
	}
	// The range constraints a library dependency carries name no build.
	for _, finding := range result.Findings {
		if strings.Contains(finding.File, "library.codefly.yaml") {
			t.Errorf("a dependency range was read as a version: %s %s=%s", finding.Location(), finding.Key, finding.Version)
		}
	}
}

// TestCleanModulesFailOnlyWhenGoModulesIsAskedFor states the measurement in a
// test: the clean releases do carry first-party pseudo-versions, so --go-modules
// is opt-in precisely because turning it on by default would fail them.
func TestCleanModulesFailOnlyWhenGoModulesIsAskedFor(t *testing.T) {
	result := scanFixture(t, "clean-module", Options{GoModules: true})
	if result.OK() {
		t.Fatal("--go-modules did not refuse the first-party pseudo-versions")
	}
	for _, finding := range result.Blocking() {
		if finding.Carrier != CarrierGoModule {
			t.Errorf("%s: --go-modules promoted an unrelated carrier %q", finding.Location(), finding.Carrier)
		}
	}
}

// TestThirdPartyPseudoVersionsAreNotOurBusiness: a pseudo-version of a dependency
// this fleet does not publish is somebody else's release cadence.
func TestThirdPartyPseudoVersionsAreNotOurBusiness(t *testing.T) {
	result := scanFixture(t, "module-with-dev-pins", Options{GoModules: true})
	for _, finding := range result.Findings {
		if strings.Contains(finding.Key, "google.golang.org/grpc") {
			t.Errorf("flagged a third-party pseudo-version: %s %s", finding.Location(), finding.Key)
		}
	}
	var sawFirstParty bool
	for _, finding := range result.Findings {
		if finding.Carrier == CarrierGoModule && strings.Contains(finding.Key, "acme/platform-core") {
			sawFirstParty = true
		}
	}
	if !sawFirstParty {
		t.Error("the first-party pseudo-version in the same file was not found, so the previous check passes vacuously")
	}
}

// TestLabelledAgentOverrideIsPermittedOnMainAndRefusedForARelease is the design
// decision, stated as behaviour: a documented dev loop keeps working on the
// default branch, and the release scope is what guarantees the property that
// actually broke — a released tag free of prereleases.
func TestLabelledAgentOverrideIsPermittedOnMainAndRefusedForARelease(t *testing.T) {
	onMain := scanFixture(t, "composition", Options{})
	wantBlocking := []string{
		"workspace.codefly.yaml:11 modules[2].version=0.1.8-rc.1",
		"workspace.codefly.yaml:14 solutions[0].version=0.0.0-20260930123456-abcdef123456",
		"workspace.codefly.yaml:25 agent-overrides.codefly.dev/nextjs=0.0.159-dev.1ed5001fd8b4",
		"workspace.codefly.yaml:28 agent-overrides.codefly.dev/python-fastapi=0.0.110-dev.d7feea072dc8",
	}
	if got := findingKeys(onMain.Blocking()); strings.Join(got, "\n") != strings.Join(wantBlocking, "\n") {
		t.Errorf("default-branch blocking:\ngot:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(wantBlocking, "\n  "))
	}
	wantAllowed := []string{
		"workspace.codefly.yaml:20 agent-overrides.codefly.dev/go=0.0.63-dev.bd71dd90cc10",
		"workspace.codefly.yaml:24 agent-overrides.codefly.dev/go-grpc=0.1.48-dev.42ce38050224",
	}
	if got := findingKeys(onMain.Allowed()); strings.Join(got, "\n") != strings.Join(wantAllowed, "\n") {
		t.Errorf("default-branch allowed:\ngot:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(wantAllowed, "\n  "))
	}

	forRelease := scanFixture(t, "composition", Options{Release: true})
	if len(forRelease.Allowed()) != 0 {
		t.Errorf("release scope permitted %d prerelease(s); a tag must contain none", len(forRelease.Allowed()))
	}
	if len(forRelease.Blocking()) != len(onMain.Findings) {
		t.Errorf("release scope refused %d of %d findings", len(forRelease.Blocking()), len(onMain.Findings))
	}
	// The released redis override is untouched in both scopes.
	for _, finding := range forRelease.Findings {
		if strings.Contains(finding.Key, "redis") {
			t.Errorf("a released agent-overrides entry was flagged: %s", finding.Location())
		}
	}
}

// TestFailureNamesTheFileTheKeyAndTheVersion: the person who wrote the pin had no
// reason to know a downstream composition's release doc depended on it, so the
// output has to carry the whole story.
func TestFailureNamesTheFileTheKeyAndTheVersion(t *testing.T) {
	failure := scanFixture(t, "module-with-dev-pins", Options{}).Failure()
	if failure == nil {
		t.Fatal("no failure")
	}
	message := failure.Error()
	for _, fragment := range []string{
		"module/services/frontend/service.codefly.yaml:6",
		"agent.version = 0.0.159-dev.1ed5001fd8b4",
		"prerelease-tag",
		"codefly update workspace",
		"agent-overrides",
		"codefly.local.yaml",
	} {
		if !strings.Contains(message, fragment) {
			t.Errorf("failure output does not mention %q:\n%s", fragment, message)
		}
	}
}

// TestUnlabelledOverrideIsToldWhatToDo: an unlabelled override is one comment
// away from passing, and the output has to say so rather than just refusing.
func TestUnlabelledOverrideIsToldWhatToDo(t *testing.T) {
	failure := scanFixture(t, "composition", Options{}).Failure()
	if failure == nil {
		t.Fatal("the unlabelled override was not refused")
	}
	message := failure.Error()
	if !strings.Contains(message, "naming the issue it stands in for") {
		t.Errorf("the unlabelled override was refused without saying what a label is:\n%s", message)
	}
}

// TestPassingScanReportsPermittedPrereleasesAnyway: an allowed prerelease is
// still a prerelease somebody has to remove, so a green run says so.
func TestPassingScanReportsPermittedPrereleasesAnyway(t *testing.T) {
	result := scanFixture(t, "clean-module", Options{})
	if !strings.Contains(result.Summary(), "permitted prerelease") {
		t.Errorf("a passing scan hid its permitted prereleases: %q", result.Summary())
	}
}

// TestJSONReportCarriesBothSets keeps the machine-readable form honest: a CI job
// reading it sees the allowed findings too, not only the fatal ones.
func TestJSONReportCarriesBothSets(t *testing.T) {
	payload, err := scanFixture(t, "composition", Options{}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"status": "fail"`, `"scope": "default-branch"`, `"blocking"`, `"allowed"`, "agent-overrides.codefly.dev/go"} {
		if !strings.Contains(string(payload), fragment) {
			t.Errorf("JSON report is missing %q:\n%s", fragment, payload)
		}
	}
}

// TestJSONReportRendersEmptyListsAsLists: the clean case is the one a CI job sees
// most, and `.blocking | length` should not have to special-case null for it.
func TestJSONReportRendersEmptyListsAsLists(t *testing.T) {
	payload, err := scanFixture(t, "clean-module", Options{}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "null") {
		t.Errorf("an empty list rendered as null:\n%s", payload)
	}
	if !strings.Contains(string(payload), `"blocking": []`) {
		t.Errorf("blocking is not an empty list:\n%s", payload)
	}
	if !strings.Contains(string(payload), `"status": "pass"`) {
		t.Errorf("status is not pass:\n%s", payload)
	}
}

// TestDiscoverReadsWhatGitTracks pins the discovery semantics, which are the
// point rather than an optimisation: the rule is about what reaches the default
// branch, and what reaches it is what git tracks. That is also why the local dev
// loop needs no exception — codefly.local.yaml is gitignored, so it is outside
// the gate by construction.
//
// It doubles as the guard for the trap that follows from it: a fixture file that
// has not been `git add`ed is invisible to every test in this package, which
// would otherwise show up as a fixture that mysteriously stopped carrying
// findings.
func TestDiscoverReadsWhatGitTracks(t *testing.T) {
	for _, fixture := range []string{"module-with-dev-pins", "clean-module", "composition"} {
		dir := filepath.Join("testdata", fixture)
		tracked, fromGit, err := Discover(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !fromGit {
			t.Fatalf("%s is inside this repository, so Discover should have enumerated it from git", fixture)
		}
		for _, file := range tracked {
			if !isCodeflyConfig(file) && filepath.Base(file) != "go.mod" {
				t.Errorf("%s: Discover returned %q, which carries no version pin", fixture, file)
			}
		}
		walked, err := walkFixture(dir)
		if err != nil {
			t.Fatal(err)
		}
		if missing := difference(walked, tracked); len(missing) > 0 {
			t.Errorf("%s: %v exist on disk but git does not track them, so no test in this package can see them — `git add` them", fixture, missing)
		}
	}
}

// walkFixture is the same file set Discover would produce outside a repository,
// used only to tell "git does not track this" from "this file does not exist".
func walkFixture(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(dir, full)
		if relErr != nil {
			return relErr
		}
		return nil2(&files, filepath.ToSlash(relative))
	})
	if err != nil {
		return nil, err
	}
	return keepScannable(files), nil
}

func nil2(files *[]string, file string) error {
	*files = append(*files, file)
	return nil
}

func difference(all, subset []string) []string {
	have := make(map[string]bool, len(subset))
	for _, file := range subset {
		have[file] = true
	}
	var missing []string
	for _, file := range all {
		if !have[file] {
			missing = append(missing, file)
		}
	}
	return missing
}

// TestTestdataIsSkippedUnlessAskedFor: a fixture's job can be to carry a bad
// pin — this package's own do — so a gate that read testdata could not run on the
// repository that implements it. The flag exists so the exclusion is a choice
// rather than a blind spot, and this pins both halves.
func TestTestdataIsSkippedUnlessAskedFor(t *testing.T) {
	// pkg/prerelease, scanned from the package directory: everything under it is
	// testdata, and all of it carries prereleases on purpose.
	skipped, err := Scan(".", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !skipped.OK() {
		t.Errorf("the package's own fixtures were read by default:\n%s", skipped.Report())
	}
	if len(skipped.Files) != 0 {
		t.Errorf("scanned %v, all of which is testdata", skipped.Files)
	}

	included, err := Scan(".", Options{IncludeTestdata: true})
	if err != nil {
		t.Fatal(err)
	}
	if included.OK() {
		t.Error("--include-testdata did not read the fixtures, so the exclusion above passes vacuously")
	}
}

// TestScanNeedsNoWorkspaceAgentOrNetwork is the property that makes this runnable
// on every pull request: a directory holding nothing but one file still scans.
func TestScanNeedsNoWorkspaceAgentOrNetwork(t *testing.T) {
	dir := t.TempDir()
	const manifest = "name: store\nagent:\n    name: postgres\n    version: 0.0.139-dev.9785d12f3700\n"
	if err := os.WriteFile(filepath.Join(dir, "service.codefly.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Scan(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tracked {
		t.Error("a bare temporary directory is not a git work tree")
	}
	if len(result.Blocking()) != 1 {
		t.Fatalf("got %d blocking findings, want 1:\n%s", len(result.Blocking()), result.Report())
	}
	if got := result.Blocking()[0].Location(); got != "service.codefly.yaml:4" {
		t.Errorf("location %q", got)
	}
}

// TestMalformedFilesAreLeftToTheCommandThatLoadsThem: turning an unrelated YAML
// or go.mod error into "prerelease check failed" would send the reader after the
// wrong thing, and the command that actually loads the file reports it with the
// schema context this scan does not have.
func TestMalformedFilesAreLeftToTheCommandThatLoadsThem(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"service.codefly.yaml": "name: broken\n  agent:\n version: [unclosed\n",
		"go.mod":               "this is not a go.mod\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Scan(dir, Options{GoModules: true})
	if err != nil {
		t.Fatalf("a malformed file failed the scan: %v", err)
	}
	if len(result.Findings) != 0 {
		t.Errorf("got findings from malformed files: %v", result.Findings)
	}
}

// TestFirstPartyComesFromTheRepositoryNotFromTheCLI is the correction this file
// exists to hold. The gate shipped with two GitHub organisations hardcoded in the
// binary, which was wrong in kind: a generic tool cannot name the products that
// use it, the list is stale the moment somebody adds an organisation, and every
// repository that is not one of those two silently got a narrower check.
//
// First-party is now whatever owner the scanned repository's own go.mod module
// paths publish under — so a repository the CLI has never heard of gets the same
// check as one it was written against.
func TestFirstPartyComesFromTheRepositoryNotFromTheCLI(t *testing.T) {
	result := scanFixture(t, "clean-module", Options{GoModules: true})
	if want := []string{"github.com/acme/"}; len(result.FirstParty) != 1 || result.FirstParty[0] != want[0] {
		t.Fatalf("derived first-party %v, want %v — the fixture's go.mod publishes under github.com/acme/", result.FirstParty, want)
	}
	if len(result.Blocking()) == 0 {
		t.Fatal("the derived owner matched nothing, so the derivation is not actually in use")
	}
	for _, finding := range result.Blocking() {
		if !strings.Contains(finding.Key, "github.com/acme/") {
			t.Errorf("%s: judged %q first-party, which the fixture does not publish", finding.Location(), finding.Key)
		}
	}

	// Nothing in the binary names an owner: a repository publishing under an owner
	// no fixture mentions is judged by its own go.mod all the same.
	dir := t.TempDir()
	const mod = "module example.test/someone-else/thing\n\ngo 1.26\n\nrequire example.test/someone-else/lib v0.0.0-20260930123456-abcdef123456\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := Scan(dir, Options{GoModules: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(elsewhere.Blocking()) != 1 {
		t.Fatalf("an unrelated owner's repository got %d findings, want 1:\n%s", len(elsewhere.Blocking()), elsewhere.Report())
	}
	if got := elsewhere.FirstParty; len(got) != 1 || got[0] != "example.test/someone-else/" {
		t.Errorf("derived %v", got)
	}
}

// TestExplicitFirstPartyOverridesTheDerivation, for a repository whose siblings
// are published under an owner it does not itself publish under.
func TestExplicitFirstPartyOverridesTheDerivation(t *testing.T) {
	result := scanFixture(t, "clean-module", Options{GoModules: true, FirstParty: []string{"github.com/nobody/"}})
	if !result.OK() {
		t.Errorf("an explicit prefix that matches nothing still produced findings:\n%s", result.Report())
	}
	if len(result.FirstParty) != 1 || result.FirstParty[0] != "github.com/nobody/" {
		t.Errorf("FirstParty is %v, want the caller's value", result.FirstParty)
	}
}

// TestOwnerPrefixNeedsAnOwnerSegment: a module path with nothing to take an owner
// from contributes no prefix, rather than a prefix that matches half the world.
func TestOwnerPrefixNeedsAnOwnerSegment(t *testing.T) {
	for path, want := range map[string]string{
		"github.com/acme/widgets":                   "github.com/acme/",
		"github.com/acme/widgets/services/api/code": "github.com/acme/",
		"example.test/someone-else/thing":           "example.test/someone-else/",
		"example.com/thing":                         "",
		"localmodule":                               "",
		"":                                          "",
	} {
		if got := ownerPrefix(path); got != want {
			t.Errorf("ownerPrefix(%q) = %q, want %q", path, got, want)
		}
	}
	// The trailing slash is load-bearing: without it one owner's prefix matches
	// another whose name merely starts the same way.
	if isFirstParty("github.com/acme-corp/widgets", []string{ownerPrefix("github.com/acme/widgets")}) {
		t.Error("github.com/acme/ matched github.com/acme-corp/")
	}
}
