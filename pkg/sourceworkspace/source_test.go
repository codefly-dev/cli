package sourceworkspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"golang.org/x/mod/modfile"
)

func TestPrepareModelsGoCheckoutAsPluginSourceResource(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module example.com/source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(context.Background(), source)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Close()
	if prepared.Service.Agent == nil || prepared.Service.Agent.Name != "go" || prepared.Service.Agent.Version != GenericGoPluginVersion {
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

func TestSelectPluginCoversFixerLanguages(t *testing.T) {
	tests := []struct {
		marker string
		name   string
	}{
		{marker: "go.mod", name: "go"},
		{marker: "pyproject.toml", name: "python"},
		{marker: "uv.lock", name: "python"},
		{marker: pythonSetupMarker, name: "python"},
		{marker: "setup.cfg", name: "python"},
		{marker: "requirements.in", name: "python"},
		{marker: "requirements.txt", name: "python"},
		{marker: "package.json", name: "nextjs"},
		{marker: "Cargo.toml", name: "rust"},
		{marker: "Package.swift", name: "swift"},
	}
	for _, test := range tests {
		t.Run(test.marker, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, test.marker), []byte("marker"), 0o644); err != nil {
				t.Fatal(err)
			}
			plugin, err := SelectPlugin(dir)
			if err != nil {
				t.Fatal(err)
			}
			if plugin.Name != test.name || plugin.Version == "" {
				t.Fatalf("plugin = %+v", plugin)
			}
		})
	}
}

func TestSelectPluginPrefersPythonPackageOverFrontendManifest(t *testing.T) {
	dir := t.TempDir()
	for _, marker := range []string{pythonSetupMarker, "package.json"} {
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("marker"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plugin, err := SelectPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Name != "python" {
		t.Fatalf("plugin = %s, want python", plugin.Name)
	}
}

func TestSelectPluginUsesMarkerlessSourceEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "dependency"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "dependency", "index.js"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin, err := SelectPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Name != "python" {
		t.Fatalf("plugin = %s, want python", plugin.Name)
	}
}

func TestSelectPluginFallsBackToGenericWithoutInterpretingUnknownLanguages(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "empty source tree"},
		{name: "unknown build manifest", file: "pom.xml"},
		{name: "unknown project declaration", file: "Cart.csproj"},
		{name: "unknown source extension", file: "main.zig"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.file != "" {
				if err := os.WriteFile(filepath.Join(dir, test.file), []byte("source evidence\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			plugin, err := SelectPlugin(dir)
			if err != nil {
				t.Fatal(err)
			}
			if plugin.Publisher != "codefly.dev" || plugin.Name != "generic" || plugin.Version != GenericPluginVersion {
				t.Fatalf("plugin = %+v, want pinned language-neutral fallback", plugin)
			}
		})
	}
}

func TestPrepareWithAgentDoesNotRequireLanguageMarkers(t *testing.T) {
	source := t.TempDir()
	prepared, err := PrepareWithAgent(context.Background(), source, &resources.Agent{
		Kind: resources.ServiceAgent, Publisher: "codefly.dev",
		Name: "go", Version: GenericGoPluginVersion,
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

	prepared, err := Prepare(context.Background(), source)
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

func TestWriteNormalizedGoWorkspaceDropsUnavailableExternalPaths(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	workFile := filepath.Join(root, "go.work")
	if err := os.WriteFile(workFile, []byte(`go 1.25

use (
	./module
	./missing-module
)

replace example.com/dependency => ./missing-replacement
`), 0o644); err != nil {
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
		t.Fatalf("normalized uses = %+v, want available module %q\n%s", parsed.Use, physicalModule, payload)
	}
	if len(parsed.Replace) != 0 {
		t.Fatalf("normalized replacements = %+v, want unavailable replacement omitted\n%s", parsed.Replace, payload)
	}
}
