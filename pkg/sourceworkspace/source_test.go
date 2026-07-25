package sourceworkspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
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
		{marker: "package.json", name: "nextjs"},
		{marker: "Cargo.toml", name: "rust"},
		{marker: "Package.swift", name: "swift"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
	if goWorkspaceIncludes(unlisted) {
		t.Fatal("unlisted module inherited an enclosing go.work")
	}
}
