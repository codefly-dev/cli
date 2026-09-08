package run

import (
	"os"
	"path/filepath"
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
// silently leaves every consumed route unrouted. The override travels through
// the same --set seam the run path already parses, so assert it round-trips to
// the service-entry's environment rather than just eyeballing the string.
func TestSolutionConsumesOverride(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	override, err := solutionConsumesOverride(dir, "wiki/backend")
	if err != nil {
		t.Fatalf("solutionConsumesOverride: %v", err)
	}
	if override == "" {
		t.Fatal("expected an override for a solution that declares api.consumes")
	}

	parsed, err := parseSetOverrides([]string{override})
	if err != nil {
		t.Fatalf("override %q does not parse as a --set entry: %v", override, err)
	}
	value, ok := parsed["backend"][manifest.APIConsumesEnvironmentVariable]
	if !ok {
		t.Fatalf("override %q does not target backend's %s, got %v", override, manifest.APIConsumesEnvironmentVariable, parsed)
	}

	consumed, err := manifest.ParseConsumedAPIs(value)
	if err != nil {
		t.Fatalf("cannot decode %s value %q: %v", manifest.APIConsumesEnvironmentVariable, value, err)
	}
	if len(consumed) != 1 {
		t.Fatalf("expected 1 consumed API, got %d (%v)", len(consumed), consumed)
	}
	want := manifest.ConsumedAPI{
		ID: "documents", Module: "documents", Service: "api",
		Endpoint: "connect", Protocol: "connect", As: "documents",
	}
	if consumed[0] != want {
		t.Fatalf("consumed API mismatch: got %+v, want %+v", consumed[0], want)
	}
}

// Solutions that federate nothing must run exactly as before: no manifest at
// all, or a manifest whose api.consumes names no producing endpoint, injects
// no variable.
func TestSolutionConsumesOverrideNoOps(t *testing.T) {
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
			override, err := solutionConsumesOverride(dir, "wiki/backend")
			if err != nil {
				t.Fatalf("solutionConsumesOverride: %v", err)
			}
			if override != "" {
				t.Fatalf("expected no override, got %q", override)
			}
		})
	}
}

// A solution manifest that does not load is the solution's own identity file
// being broken: surfacing it beats booting a backend that silently federates
// nothing, which is the failure this injection exists to prevent.
func TestSolutionConsumesOverrideRejectsBadManifest(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, "schema_version: codefly.solution-manifest/v9\n")

	if _, err := solutionConsumesOverride(dir, "wiki/backend"); err == nil {
		t.Fatal("expected an error for an unloadable solution manifest")
	}
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
