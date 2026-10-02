package solutionrun

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

func strptr(s string) *string { return &s }

// A solution composes the saas host, which declares its own service-entry
// (frontend). The solution root must be the workspace's own `path: .` module,
// not the composed dependency — otherwise entry resolution sees two
// service-entries and reports an ambiguous root.
func TestRootRef(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace *resources.Workspace
		want      string // "" means nil expected
	}{
		{
			name: "self identified by path: .",
			workspace: &resources.Workspace{
				Name: "lastlogin-go",
				Modules: []*resources.ModuleReference{
					{Name: "lastlogin-go", PathOverride: strptr(".")},
					{Name: "saas-starter", PathOverride: strptr("../../../module-saas-starter/module")},
				},
			},
			want: "lastlogin-go",
		},
		{
			name: "path: . wins even when listed after the dependency",
			workspace: &resources.Workspace{
				Name: "wiki",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
					{Name: "documents"},
					{Name: "wiki", PathOverride: strptr(".")},
				},
			},
			want: "wiki",
		},
		{
			name: "falls back to name == workspace when no explicit path: .",
			workspace: &resources.Workspace{
				Name: "lastlogin-python",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
					{Name: "lastlogin-python"},
				},
			},
			want: "lastlogin-python",
		},
		{
			name: "no self module present",
			workspace: &resources.Workspace{
				Name: "orphan",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
				},
			},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RootRef(tc.workspace)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("expected nil root, got %q", got.Name)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected root %q, got nil", tc.want)
			}
			if got.Name != tc.want {
				t.Fatalf("expected root %q, got %q", tc.want, got.Name)
			}
		})
	}
}

// A solution declaring api.consumes must boot its backend with
// CODEFLY__API_CONSUMES populated: the solution runtime reads it to register
// each consumed module's upstream with the gateway, so an absent variable
// silently leaves every consumed route unrouted.
func TestEntryConsumes(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	consumed, value, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
	if err != nil {
		t.Fatalf("entryConsumes: %v", err)
	}
	want := manifest.ConsumedAPI{
		ID: "documents", Module: "documents", Service: "api",
		Endpoint: "connect", Protocol: "connect", As: "documents",
	}
	if len(consumed) != 1 || consumed[0] != want {
		t.Fatalf("consumed APIs mismatch: got %+v, want [%+v]", consumed, want)
	}
	decoded, err := manifest.ParseConsumedAPIs(value)
	if err != nil {
		t.Fatalf("cannot decode %s value %q: %v", manifest.APIConsumesEnvironmentVariable, value, err)
	}
	if len(decoded) != 1 || decoded[0] != want {
		t.Fatalf("env value does not round-trip: got %+v", decoded)
	}
}

// The manifest is the module's own: it is read from the module's directory and
// only for that module's service-entry. Any other service — a sibling of the
// entry, or the entry of a module whose directory ships no manifest — must come
// back empty, or one solution's consumes would bind to another backend.
func TestEntryConsumesOnlyTargetsTheModulesOwnEntry(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)
	elsewhere := t.TempDir()

	for _, tc := range []struct {
		name    string
		module  *resources.Module
		service *resources.Service
	}{
		{
			name:    "a non-entry service of the solution module",
			module:  wikiModuleAt(dir),
			service: wikiService("worker"),
		},
		{
			name: "a same-named entry of a module shipping no manifest",
			module: func() *resources.Module {
				module := &resources.Module{Name: "documents", ServiceEntry: "backend"}
				module.WithDir(elsewhere)
				return module
			}(),
			service: wikiService("backend"),
		},
		{
			name:    "a module with no directory to ship a manifest in",
			module:  &resources.Module{Name: "wiki", ServiceEntry: "backend"},
			service: wikiService("backend"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumed, value, err := entryConsumes(tc.module, tc.service)
			if err != nil {
				t.Fatalf("entryConsumes: %v", err)
			}
			if len(consumed) != 0 || value != "" {
				t.Fatalf("expected no injection, got %+v / %q", consumed, value)
			}
		})
	}
}

// A solution composed by source and version is never the workspace's own
// module: its manifest sits in its cache checkout, not at the workspace root.
// The manifest a run reads is the module's, wherever the module is — gating it
// on the workspace root left a composed solution with no projection at all.
func TestEntryConsumesIsLocatedByTheModuleNotTheWorkspace(t *testing.T) {
	checkout := t.TempDir()
	writeSolutionManifest(t, checkout, solutionManifestWithConsumes)
	workspaceDir := t.TempDir()
	writeSolutionManifest(t, workspaceDir, solutionManifestWithoutConsumes)

	composed := &resources.Module{Name: "lastlogin", ServiceEntry: "backend"}
	composed.WithDir(checkout)
	consumed, value, err := entryConsumes(composed, wikiService("backend"))
	if err != nil {
		t.Fatalf("entryConsumes: %v", err)
	}
	if len(consumed) != 1 || consumed[0].Module != "documents" || value == "" {
		t.Fatalf("the composed module's own manifest was not read: got %+v / %q", consumed, value)
	}
}

// Solutions that federate nothing must run exactly as before.
func TestEntryConsumesNoOps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string // "" means write no manifest file
	}{
		{name: "no solution manifest"},
		{name: "no api.consumes", manifest: solutionManifestWithoutConsumes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.manifest != "" {
				writeSolutionManifest(t, dir, tc.manifest)
			}
			consumed, value, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
			if err != nil {
				t.Fatalf("entryConsumes: %v", err)
			}
			if len(consumed) != 0 || value != "" {
				t.Fatalf("expected no injection, got %+v / %q", consumed, value)
			}
		})
	}
}

// Running a solution must not require the whole manifest schema: a field from a
// newer core, or a rule unrelated to federation, cannot be allowed to make the
// solution unrunnable. Only the api.consumes projection is load-bearing here.
func TestEntryConsumesToleratesUnrelatedSchemaDrift(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{"field from a newer core", solutionManifestWithConsumes + "\nfuture_field:\n  nested: true\n"},
		{"unknown key inside a consumes entry", strings.Replace(solutionManifestWithConsumes, "      as: documents", "      as: documents\n      timeout: 30s", 1)},
		{"schema version this CLI predates", strings.Replace(solutionManifestWithConsumes, "codefly.solution-manifest/v0", "codefly.solution-manifest/v1", 1)},
		{"agent block that would fail strict validation", strings.Replace(solutionManifestWithConsumes, "  version: 0.1.0", "  version: latest", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSolutionManifest(t, dir, tc.manifest)
			consumed, value, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
			if err != nil {
				t.Fatalf("schema drift must not block the run: %v", err)
			}
			if len(consumed) != 1 || value == "" {
				t.Fatalf("expected the consumes projection to survive, got %+v / %q", consumed, value)
			}
		})
	}
}

// A half-bound consumes entry names a module but no service or endpoint, which
// projects into a CODEFLY__ENDPOINT key built from empty segments. The lenient
// decode skips manifest.Validate, so this is checked here instead.
func TestEntryConsumesRejectsPartialBinding(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, "      service: api\n", "", 1))

	if _, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend")); err == nil {
		t.Fatal("expected an error for a partially bound api.consumes entry")
	}
}

// One facade prefix is one route, so two entries claiming the same prefix leave
// the runtime unable to say which module /v1/<prefix>/* reaches.
// manifest.Validate catches this for sync and package; the run's lenient decode
// skips it, so the run path is the one that must not let the typo through.
func TestEntryConsumesRejectsADuplicatedFacadePrefix(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, `lifecycle:`, `    - id: archives
      protocol: connect
      module: archives
      service: api
      endpoint: connect
      as: documents
lifecycle:`, 1))

	_, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
	if err == nil {
		t.Fatal("expected an error for two api.consumes entries claiming one facade prefix")
	}
	if !strings.Contains(err.Error(), "documents") {
		t.Fatalf("error %q does not name the duplicated prefix", err)
	}
}

// YAML that does not parse at all is a genuine boundary failure: booting a
// backend that silently federates nothing is the bug this injection exists to
// prevent.
func TestEntryConsumesRejectsUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, "api:\n\tconsumes: [oops\n")

	if _, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend")); err == nil {
		t.Fatal("expected an error for an unparseable solution manifest")
	}
}

// wikiModuleAt is the wiki solution module as a run loads it: with the directory
// its solution manifest sits in.
func wikiModuleAt(dir string) *resources.Module {
	module := &resources.Module{Name: "wiki", ServiceEntry: "backend"}
	module.WithDir(dir)
	return module
}

// wikiModuleIn is the wiki module of a loaded workspace where it is the `path: .`
// module, so its directory is the workspace's.
func wikiModuleIn(workspace *resources.Workspace) *resources.Module {
	return wikiModuleAt(workspace.Dir())
}

// entryConsumes is what DerivedRunInputs reads from the entry manifest: the
// projection and its CODEFLY__API_CONSUMES value.
func entryConsumes(module *resources.Module, service *resources.Service) ([]manifest.ConsumedAPI, string, error) {
	solutionManifest, err := entryManifest(module, service)
	if err != nil || solutionManifest == nil {
		return nil, "", err
	}
	consumed := solutionManifest.ConsumedAPIs()
	if len(consumed) == 0 {
		return nil, "", nil
	}
	return consumed, solutionManifest.ConsumedAPIsEnvValue(), nil
}

func wikiService(name string) *resources.Service {
	return &resources.Service{Name: name}
}

func writeSolutionManifest(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", manifest.FileName, err)
	}
}

const solutionManifestWithoutConsumes = `
schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: wiki
  version: 0.1.0
api:
  exposes:
    - id: gateway
      protocol: http
lifecycle:
  create: true
`

const solutionManifestWithConsumes = `
schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: wiki
  version: 0.1.0
api:
  exposes:
    - id: gateway
      protocol: http
  consumes:
    - id: documents
      protocol: connect
      module: documents
      service: api
      endpoint: connect
      as: documents
lifecycle:
  create: true
`

// The whole injection, end to end on a real workspace: the entry gets the
// projection, keyed by its module-qualified unique so it lands on exactly one
// service, and no other service of the composition receives anything.
func TestDerivedRunInputsProjectsConsumesIntoTheEntryOnly(t *testing.T) {
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	module := wikiModuleIn(workspace)
	solutionManifest, err := moduleManifest(module)
	if err != nil {
		t.Fatal(err)
	}

	derived, err := DerivedRunInputs(module, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	want := map[string]map[string]string{
		"wiki/backend": {manifest.APIConsumesEnvironmentVariable: solutionManifest.ConsumedAPIsEnvValue()},
	}
	if !reflect.DeepEqual(derived.Overrides, want) {
		t.Fatalf("overrides = %+v, want the projection on the entry alone: %+v", derived.Overrides, want)
	}
	if len(derived.Notes) != 1 || derived.Notes[0].Warning || !strings.Contains(derived.Notes[0].Message, "wiki/backend") {
		t.Errorf("notes = %+v, want one statement naming the entry", derived.Notes)
	}
}

// A solution that federates nothing must run exactly as before: no projection,
// no override on any service and nothing to tell.
func TestDerivedRunInputsNoOpsWithoutConsumes(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithoutConsumes)

	derived, err := DerivedRunInputs(wikiModuleAt(dir), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	if derived.Overrides != nil || len(derived.Notes) != 0 {
		t.Fatalf("expected no derived inputs, got %+v", derived)
	}
}

// The derivation reports what it supplied instead of printing it. pkg/control
// calls it from a process whose stdout carries JSON-RPC — the MCP server serves
// on it — so a narration line written from here corrupts that stream. The notes
// must come back with the inputs, and nothing may reach stdout.
func TestDerivedRunInputsReportsNotesWithoutPrinting(t *testing.T) {
	workspace := loadTestWorkspace(t, "testdata/solution-federation")

	var derived RunInputs
	var derivedErr error
	stdout := captureStdout(t, func() {
		derived, derivedErr = DerivedRunInputs(wikiModuleIn(workspace), wikiService("backend"), "wiki/backend")
	})
	if derivedErr != nil {
		t.Fatalf("DerivedRunInputs: %v", derivedErr)
	}
	if stdout != "" {
		t.Errorf("the derivation wrote %q to stdout; narration must be returned, not printed", stdout)
	}
	if len(derived.Notes) == 0 {
		t.Fatal("the derivation returned no notes: an operator cannot see that the CLI supplied the value")
	}
	if !strings.Contains(derived.Notes[0].Message, manifest.APIConsumesEnvironmentVariable) {
		t.Errorf("first note %q does not report the projection it injected", derived.Notes[0].Message)
	}
	for _, note := range derived.Notes {
		if note.Warning {
			t.Errorf("a solution that projects cleanly reported a warning: %q", note.Message)
		}
	}
}

// captureStdout swaps os.Stdout for the duration of fn. cli.Info resolves
// os.Stdout at call time, so this observes exactly what a print from inside the
// derivation would put on the protocol stream.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	collected := make(chan string, 1)
	go func() {
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, reader)
		collected <- buffer.String()
	}()
	fn()
	os.Stdout = original
	_ = writer.Close()
	written := <-collected
	_ = reader.Close()
	return written
}

func loadTestWorkspace(t *testing.T, dir string) *resources.Workspace {
	t.Helper()
	absolute, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), absolute)
	if err != nil {
		t.Fatalf("cannot load workspace %s: %v", dir, err)
	}
	return workspace
}

// The derivation for a solution composed by source and version: its module is
// not the workspace's own and its manifest sits in its own checkout, and it
// must still get the projection — here of the host's own accounts API, which
// is a route like any other.
func TestDerivedRunInputsForAComposedSolution(t *testing.T) {
	workspace := loadTestWorkspace(t, "testdata/solution-composed")
	if self := RootRef(workspace); self != nil {
		t.Fatalf("the fixture must have no self-root module for this to prove anything, got %q", self.Name)
	}
	module := loadTestModule(t, workspace, "consumer")

	derived, err := DerivedRunInputs(module, wikiService("backend"), "consumer/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	decoded, err := manifest.ParseConsumedAPIs(derived.Overrides["consumer/backend"][manifest.APIConsumesEnvironmentVariable])
	if err != nil {
		t.Fatalf("the composed entry's %s does not decode: %v", manifest.APIConsumesEnvironmentVariable, err)
	}
	if len(decoded) != 1 || decoded[0].Module != "host" || decoded[0].As != "accounts" {
		t.Errorf("the composed entry received %+v, want the host's accounts API", decoded)
	}
}

// Two solutions named as co-roots of one run each derive their own inputs. The
// merge layers overrides key by key, the later root winning, and keeps the
// notes in root order; merging nothing yields nothing.
func TestMergeLayersOverridesAcrossRoots(t *testing.T) {
	first := RunInputs{
		Overrides: map[string]map[string]string{"wiki/backend": {"A": "1"}},
		Notes:     []Note{{Message: "first"}},
	}
	second := RunInputs{
		Overrides: map[string]map[string]string{"notes/backend": {"B": "2"}, "wiki/backend": {"A": "3"}},
		Notes:     []Note{{Message: "second"}},
	}
	merged := Merge(first, second)
	if merged.Overrides["wiki/backend"]["A"] != "3" || merged.Overrides["notes/backend"]["B"] != "2" {
		t.Errorf("overrides did not layer key by key: %+v", merged.Overrides)
	}
	if first.Overrides["wiki/backend"]["A"] != "1" {
		t.Error("merging wrote through into the first root's overrides")
	}
	if len(merged.Notes) != 2 || merged.Notes[0].Message != "first" || merged.Notes[1].Message != "second" {
		t.Errorf("notes lost their order: %+v", merged.Notes)
	}
	if empty := Merge(RunInputs{}, RunInputs{}); empty.Overrides != nil {
		t.Errorf("merging nothing produced inputs: %+v", empty)
	}
}

// Among the modules declaring a service-entry, the root is the one nothing else
// in that set depends on. The host is depended on by the app, through a module
// with no entry of its own, and by the consumer, through api.consumes alone —
// so it is never a root, whichever of the two is composed beside it.
func TestSolutionRootsExcludesAnEntryAnotherEntryDependsOn(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-composed")
	host := loadTestModule(t, workspace, "host")
	app := loadTestModule(t, workspace, "app")
	consumer := loadTestModule(t, workspace, "consumer")

	for _, tc := range []struct {
		name    string
		entries []*resources.Module
		want    []string
	}{
		{name: "transitively through a module with no entry", entries: []*resources.Module{host, app}, want: []string{"app"}},
		{name: "through api.consumes alone", entries: []*resources.Module{consumer, host}, want: []string{"consumer"}},
		{name: "two solutions beside one host", entries: []*resources.Module{host, app, consumer}, want: []string{"app", "consumer"}},
		{name: "a lone entry", entries: []*resources.Module{host}, want: []string{"host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, root := range SolutionRoots(ctx, workspace, tc.entries) {
				got = append(got, root.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SolutionRoots = %v, want %v", got, tc.want)
			}
		})
	}
}

func loadTestModule(t *testing.T, workspace *resources.Workspace, name string) *resources.Module {
	t.Helper()
	for _, ref := range workspace.Modules {
		if ref.Name != name {
			continue
		}
		module, err := workspace.LoadModuleFromReference(context.Background(), ref)
		if err != nil {
			t.Fatalf("cannot load module %s: %v", name, err)
		}
		return module
	}
	t.Fatalf("workspace %s references no module %s", workspace.Name, name)
	return nil
}
