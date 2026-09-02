package run

import (
	"testing"

	"github.com/codefly-dev/core/resources"
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
