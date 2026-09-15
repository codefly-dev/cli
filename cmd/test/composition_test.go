package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// writeComposedWorkspace writes a workspace composing each named module at a
// directory of its own, and returns it loaded the way every command sees it.
// The map value is the module's module.codefly.yaml, or "" for a module
// directory that has none at all.
func writeComposedWorkspace(t *testing.T, modules map[string]string) *resources.Workspace {
	t.Helper()
	dir := t.TempDir()
	var refs strings.Builder
	for name := range modules {
		refs.WriteString("    - name: " + name + "\n      path: " + name + "\n")
	}
	doc := "name: solution\nlayout: modules\nmodules:\n" + refs.String()
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, descriptor := range modules {
		moduleDir := filepath.Join(dir, name)
		if err := os.MkdirAll(moduleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if descriptor == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(moduleDir, resources.ModuleConfigurationName), []byte(descriptor), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return workspace
}

const composedModuleDescriptor = `kind: composed-module
name: host
base:
  id: saas
  version: "^0.1"
contributions:
  tests:
    - path: tests/gateway
      command: ["go", "test", "./..."]
`

// A composition descriptor and an ordinary module's configuration share the
// file name module.codefly.yaml and differ only by kind. Decoding a plain
// module as a descriptor fails on its unknown fields, so a workspace of
// ordinary modules must still report no contributed tests rather than an
// error — otherwise `codefly test solution` breaks on every workspace that
// composes nothing.
func TestContributedSuitesSkipsModulesThatComposeNoPackage(t *testing.T) {
	workspace := writeComposedWorkspace(t, map[string]string{
		"plain":   "name: plain\nservices: []\n",
		"kindful": "kind: module\nname: kindful\nservices: []\n",
		"bare":    "",
	})

	suites, err := contributedSuites(context.Background(), workspace)
	if err != nil {
		t.Fatalf("contributed suites: %v", err)
	}
	if len(suites) != 0 {
		t.Fatalf("contributed suites = %v, want none for modules composing no package", suites)
	}
}

// The contributed suite runs in the contributing module's own checkout, at the
// path the descriptor declares — the same directory core's renderer runs it in.
func TestContributedSuitesCollectsDeclaredTests(t *testing.T) {
	workspace := writeComposedWorkspace(t, map[string]string{
		"host":  composedModuleDescriptor,
		"plain": "name: plain\nservices: []\n",
	})

	suites, err := contributedSuites(context.Background(), workspace)
	if err != nil {
		t.Fatalf("contributed suites: %v", err)
	}
	if len(suites) != 1 {
		t.Fatalf("contributed suites = %d, want the one the composed module declares", len(suites))
	}
	suite := suites[0]
	if suite.module != "host" {
		t.Errorf("suite module = %q, want the module contributing it", suite.module)
	}
	if suite.spec.Name != "tests/gateway" {
		t.Errorf("suite name = %q, want the declared contribution path", suite.spec.Name)
	}
	if got, want := suite.spec.Command, []string{"go", "test", "./..."}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("suite command = %v, want %v", got, want)
	}
	wantDir := filepath.Join(workspace.Dir(), "host", "tests", "gateway")
	if suite.spec.Directory != wantDir {
		t.Errorf("suite directory = %q, want %q", suite.spec.Directory, wantDir)
	}
}

// A module.codefly.yaml that cannot be parsed at all is a broken module, not an
// absent descriptor: reporting "no tests" for it would hide the defect.
func TestContributedSuitesReportsUnparseableModuleFile(t *testing.T) {
	workspace := writeComposedWorkspace(t, map[string]string{
		"broken": "kind: [unterminated\n",
	})

	_, err := contributedSuites(context.Background(), workspace)
	if err == nil {
		t.Fatal("an unparseable module.codefly.yaml was reported as contributing no tests")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error %q does not name the offending module", err)
	}
}

// A workspace contributing nothing must not fail the solution's own tests, and
// must stay silent rather than announcing an absence on every run.
func TestRunContributedCompositionTestsIsANoOpWithoutContributions(t *testing.T) {
	workspace := writeComposedWorkspace(t, map[string]string{
		"plain": "name: plain\nservices: []\n",
	})

	if err := runContributedCompositionTests(context.Background(), workspace); err != nil {
		t.Fatalf("a workspace contributing no composition tests failed: %v", err)
	}
}

// A composed module that does not resolve locally must not stop the solution
// from testing itself. Materializing pinned modules is best effort — it warns
// and carries on when a pull fails — so a workspace that could test its own
// entry before must still be able to, with the unresolvable module simply
// contributing nothing.
//
// The pinned module here declares no module-trust, which ResolvePinnedModule
// rejects before it reaches the network.
func TestContributedSuitesSkipsModulesThatDoNotResolve(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	dir := t.TempDir()
	doc := "name: solution\nlayout: modules\nmodules:\n" +
		"    - name: unresolvable\n      source: owner/absent\n      version: \"0.1.0\"\n" +
		"    - name: host\n      path: host\n"
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	hostDir := filepath.Join(dir, "host")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, resources.ModuleConfigurationName), []byte(composedModuleDescriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}

	suites, err := contributedSuites(context.Background(), workspace)
	if err != nil {
		t.Fatalf("an unresolvable composed module failed the whole sweep: %v", err)
	}
	if len(suites) != 1 || suites[0].module != "host" {
		t.Fatalf("contributed suites = %v, want only the resolvable module's suite", suites)
	}
}

const markerModuleDescriptor = `kind: composed-module
name: host
base:
  id: saas
  version: "^0.1"
contributions:
  tests:
    - path: tests/gateway
      command: ["sh", "-c", "touch ran.marker"]
`

// --load-only and --init-only bring the stack up without running tests. A
// contributed suite is a test, and an arbitrary command at that, so neither
// flag may execute one.
func TestContributedCompositionTestsDoNotRunUnderLoadOrInitOnly(t *testing.T) {
	workspace := writeComposedWorkspace(t, map[string]string{"host": markerModuleDescriptor})
	suiteDir := filepath.Join(workspace.Dir(), "host", "tests", "gateway")
	if err := os.MkdirAll(suiteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(suiteDir, "ran.marker")

	priorLoad, priorInit := loadOnly, initOnly
	t.Cleanup(func() { loadOnly, initOnly = priorLoad, priorInit })

	for _, tc := range []struct {
		flag       string
		load, init bool
	}{
		{flag: "load-only", load: true},
		{flag: "init-only", init: true},
	} {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		loadOnly, initOnly = tc.load, tc.init
		if err := runContributedCompositionTests(context.Background(), workspace); err != nil {
			t.Fatalf("--%s: %v", tc.flag, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("--%s executed a contributed suite; it must not run tests", tc.flag)
		}
	}

	// Control: with neither flag the suite does run, so the assertions above
	// fail on a missing gate rather than passing because nothing ever runs.
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	loadOnly, initOnly = false, false
	if err := runContributedCompositionTests(context.Background(), workspace); err != nil {
		t.Fatalf("contributed suite failed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the contributed suite did not run, so the gate assertions prove nothing: %v", err)
	}
}
