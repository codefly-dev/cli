package sourceworkspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"golang.org/x/mod/modfile"
)

func TestPrepareModelsGoCheckoutAsPluginSourceResource(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module example.com/source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareWithAgent(context.Background(), source, &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "custom", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Close()
	if prepared.Service.Agent == nil || prepared.Service.Agent.Name != "custom" || prepared.Service.Agent.Version != "1.2.3" {
		t.Fatalf("plugin = %+v", prepared.Service.Agent)
	}
	if prepared.Service.Spec["source-dir"] != "code" || prepared.Service.Spec["with-workspace"] != false {
		t.Fatalf("spec = %+v", prepared.Service.Spec)
	}
	if prepared.GoWorkFile != "" {
		t.Fatalf("GoWorkFile = %q, want no workspace", prepared.GoWorkFile)
	}
	linked, err := filepath.EvalSymlinks(filepath.Join(prepared.Service.Dir(), "code"))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if linked != physical {
		t.Fatalf("linked source = %s, want %s", linked, physical)
	}
}

func TestPrepareWithAgentDoesNotRequireLanguageMarkers(t *testing.T) {
	source := t.TempDir()
	prepared, err := PrepareWithAgent(context.Background(), source, &resources.Agent{
		Kind: resources.ServiceAgent, Publisher: "codefly.dev",
		Name: "go", Version: "1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Service.Agent == nil || prepared.Service.Agent.Name != "go" {
		t.Fatalf("plugin = %+v", prepared.Service.Agent)
	}
}

func TestGoWorkspaceIncludesOnlyListedModule(t *testing.T) {
	root := t.TempDir()
	included := filepath.Join(root, "included")
	unlisted := filepath.Join(root, "unlisted")
	if err := os.MkdirAll(included, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unlisted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.24\n\nuse ./included\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !goWorkspaceIncludes(included) {
		t.Fatal("listed module was not recognized as part of go.work")
	}
	wantWorkFile, err := filepath.EvalSymlinks(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := goWorkspaceFile(included), wantWorkFile; got != want {
		t.Fatalf("goWorkspaceFile = %q, want %q", got, want)
	}
	if goWorkspaceIncludes(unlisted) {
		t.Fatal("unlisted module inherited an enclosing go.work")
	}
}

// The nearest go.work that does not `use` a module is not applied to it, and
// is reported as such so a build can say it is pinned despite that file.
func TestGoWorkspaceReportsTheNearestWorkspaceThatDoesNotUseTheModule(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"included", "unlisted"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.24\n\nuse ./included\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workFile, err := filepath.EvalSymlinks(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatal(err)
	}
	if got := goWorkspaceUnused(filepath.Join(root, "unlisted")); got != workFile {
		t.Fatalf("unused workspace of the unlisted module = %q, want %q", got, workFile)
	}
	if got := goWorkspaceUnused(filepath.Join(root, "included")); got != "" {
		t.Fatalf("a governing workspace was reported as unused: %q", got)
	}
	if got := goWorkspaceUnused(t.TempDir()); got != "" {
		t.Fatalf("no workspace above, yet one is reported unused: %q", got)
	}
}

func TestPrepareCarriesExactGoWorkspaceAcrossEphemeralSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "agent")
	replacement := filepath.Join(root, "replacement")
	for _, directory := range []string{source, replacement} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module example.com/agent\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workFile := filepath.Join(root, "go.work")
	if err := os.WriteFile(workFile, []byte("go 1.25\n\nuse ./agent\n\nreplace example.com/dependency => ./replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareWithAgent(context.Background(), source, &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "custom", Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.GoWorkFile != filepath.Join(prepared.Dir, "go.work") {
		t.Fatalf("GoWorkFile = %q, want ephemeral normalized workspace", prepared.GoWorkFile)
	}
	if prepared.Service.Spec["with-workspace"] != true {
		t.Fatalf("spec = %+v, want workspace-enabled source", prepared.Service.Spec)
	}
	payload, err := os.ReadFile(prepared.GoWorkFile)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.ParseWork(prepared.GoWorkFile, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Use) != 1 {
		t.Fatalf("normalized uses = %+v", parsed.Use)
	}
	physicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Use[0].Path != filepath.ToSlash(physicalSource) {
		t.Fatalf("normalized use = %q, want %q", parsed.Use[0].Path, physicalSource)
	}
	physicalReplacement, err := filepath.EvalSymlinks(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Replace) != 1 || parsed.Replace[0].New.Path != filepath.ToSlash(physicalReplacement) {
		t.Fatalf("normalized replacements = %+v, want %q", parsed.Replace, physicalReplacement)
	}
}

// An absolute workspace member may already equal its physical normalized path.
// modfile.SetUse retains that existing entry and appends the requested entry,
// producing a go.work that Go rejects as "appears multiple times in workspace".
// Local agent builds hit this when go.work named a worktree outside its root.
func TestWriteNormalizedGoWorkspaceDoesNotDuplicateAbsolutePhysicalUse(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	workFile := filepath.Join(root, "go.work")
	if err := os.WriteFile(workFile, []byte("go 1.25\n\nuse "+filepath.ToSlash(module)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "normalized.work")
	if err := writeNormalizedGoWorkspace(workFile, destination); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.ParseWork(destination, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	physicalModule, err := filepath.EvalSymlinks(module)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Use) != 1 || parsed.Use[0].Path != filepath.ToSlash(physicalModule) {
		t.Fatalf("normalized uses = %+v, want one physical module %q\n%s", parsed.Use, physicalModule, payload)
	}
}

// A go.work member, or a local replacement, that is not on disk is refused
// naming the go.work and the member. It was dropped without a word, so the
// agent built against the PUBLISHED dependency while `go build` in the same
// checkout fails ("cannot load module ... listed in go.work"): a build that
// succeeds on a module graph nobody asked for.
func TestWriteNormalizedGoWorkspaceRefusesAMemberThatIsNotOnDisk(t *testing.T) {
	for name, tc := range map[string]struct{ body, missing string }{
		"use":         {"use (\n\t./module\n\t./missing-module\n)\n", "use ./missing-module"},
		"replacement": {"use ./module\n\nreplace example.com/dependency => ./missing-replacement\n", "replace example.com/dependency => ./missing-replacement"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "module"), 0o755); err != nil {
				t.Fatal(err)
			}
			workFile := filepath.Join(root, "go.work")
			if err := os.WriteFile(workFile, []byte("go 1.25\n\n"+tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(root, "normalized.work")
			err := writeNormalizedGoWorkspace(workFile, destination)
			if err == nil || !strings.Contains(err.Error(), workFile) || !strings.Contains(err.Error(), tc.missing) || !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("err = %v, want it to name %s and %q as missing", err, workFile, tc.missing)
			}
			if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
				t.Fatalf("a workspace was written despite the refusal: %v", statErr)
			}
		})
	}
}
