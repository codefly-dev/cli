package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

func strptr(s string) *string { return &s }

// The solution verb delegates to runServiceCommand, which folds --naming-scope
// into env.NamingScope (and thus every port hash) — but only if the flag is
// actually registered on SolutionCmd, so a solution can boot on a disjoint port
// set in parallel with another running stack. Both port-isolation flags must
// also describe the same mechanism identically to their ServiceCmd twins, or
// `--help` documents one flag two different ways.
func TestSolutionCommandExposesPortIsolationFlags(t *testing.T) {
	for _, name := range []string{"naming-scope", "temporary-ports"} {
		solutionFlag := SolutionCmd.Flags().Lookup(name)
		if solutionFlag == nil {
			t.Fatalf("run solution has no --%s flag", name)
		}
		serviceFlag := ServiceCmd.Flags().Lookup(name)
		if serviceFlag == nil {
			t.Fatalf("run service has no --%s flag", name)
		}
		if solutionFlag.Usage != serviceFlag.Usage {
			t.Fatalf("--%s help diverges: solution=%q service=%q", name, solutionFlag.Usage, serviceFlag.Usage)
		}
	}
}

// A solution composes the saas host, which declares its own service-entry
// (frontend). The solution root must be the workspace's own `path: .` module,
// not the composed dependency — otherwise resolveSolutionEntry sees two
// service-entries and reports an ambiguous root.
func TestSolutionRootRef(t *testing.T) {
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
			got := solutionRootRef(tc.workspace)
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
func TestSolutionEntryConsumes(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	consumed, value, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
	if err != nil {
		t.Fatalf("solutionEntryConsumes: %v", err)
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

// The manifest at the workspace root describes the workspace's own module.
// Injecting it into any other service binds one solution's consumes to another
// backend, so every non-entry target must come back empty.
func TestSolutionEntryConsumesOnlyTargetsTheSelfRootEntry(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	for _, tc := range []struct {
		name      string
		workspace *resources.Workspace
		module    *resources.Module
		service   *resources.Service
	}{
		{
			name:      "a non-entry service of the solution root",
			workspace: wikiWorkspace(dir),
			module:    wikiModule(),
			service:   wikiService("worker"),
		},
		{
			name:      "a same-named service in a composed module",
			workspace: wikiWorkspace(dir),
			module:    &resources.Module{Name: "documents", ServiceEntry: "backend"},
			service:   wikiService("backend"),
		},
		{
			name: "no self-root module, so the entry came from a composed module",
			workspace: workspaceAt(dir, &resources.Workspace{
				Name: "orphan",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
				},
			}),
			module:  &resources.Module{Name: "saas-starter", ServiceEntry: "backend"},
			service: wikiService("backend"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumed, value, err := solutionEntryConsumes(tc.workspace, tc.module, tc.service)
			if err != nil {
				t.Fatalf("solutionEntryConsumes: %v", err)
			}
			if len(consumed) != 0 || value != "" {
				t.Fatalf("expected no injection, got %+v / %q", consumed, value)
			}
		})
	}
}

// Solutions that federate nothing must run exactly as before.
func TestSolutionEntryConsumesNoOps(t *testing.T) {
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
			consumed, value, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
			if err != nil {
				t.Fatalf("solutionEntryConsumes: %v", err)
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
func TestSolutionEntryConsumesToleratesUnrelatedSchemaDrift(t *testing.T) {
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
			consumed, value, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
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
func TestSolutionEntryConsumesRejectsPartialBinding(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, "      service: api\n", "", 1))

	if _, _, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend")); err == nil {
		t.Fatal("expected an error for a partially bound api.consumes entry")
	}
}

// YAML that does not parse at all is a genuine boundary failure: booting a
// backend that silently federates nothing is the bug this injection exists to
// prevent.
func TestSolutionEntryConsumesRejectsUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, "api:\n\tconsumes: [oops\n")

	if _, _, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend")); err == nil {
		t.Fatal("expected an error for an unparseable solution manifest")
	}
}

func wikiWorkspace(dir string) *resources.Workspace {
	return workspaceAt(dir, &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "wiki", PathOverride: strptr(".")}},
	})
}

func workspaceAt(dir string, workspace *resources.Workspace) *resources.Workspace {
	workspace.WithDir(dir)
	return workspace
}

func wikiModule() *resources.Module {
	return &resources.Module{Name: "wiki", ServiceEntry: "backend"}
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
