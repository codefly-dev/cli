package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
)

func TestDepsCommandReturnsValidationErrors(t *testing.T) {
	if DepsCmd.RunE == nil || DepsCmd.Run != nil {
		t.Fatal("agent deps must return errors through RunE")
	}
	if err := DepsCmd.Args(DepsCmd, []string{"extra"}); err == nil {
		t.Fatal("agent deps accepted positional arguments")
	}

	previousLink, _ := DepsCmd.Flags().GetBool("link")
	previousUnlink, _ := DepsCmd.Flags().GetBool("unlink")
	if err := DepsCmd.Flags().Set("link", "true"); err != nil {
		t.Fatal(err)
	}
	if err := DepsCmd.Flags().Set("unlink", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = DepsCmd.Flags().Set("link", boolString(previousLink))
		_ = DepsCmd.Flags().Set("unlink", boolString(previousUnlink))
	})
	if err := DepsCmd.RunE(DepsCmd, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("link/unlink error = %v", err)
	}
}

func TestFindGoModDirsReturnsErrorsAndSkipsGeneratedTrees(t *testing.T) {
	if _, err := findGoModDirs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing root did not return a walk error")
	}
	root := t.TempDir()
	want := []string{root, filepath.Join(root, "nested")}
	for _, dir := range append(want, filepath.Join(root, "testdata", "fixture")) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findGoModDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("module dirs = %v, want %v", got, want)
	}
}

func TestDiscoverAgentDirsWalksRecursivelyAndSkipsTestdata(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "agents", "services", "group", "agent")
	skipped := filepath.Join(root, "testdata", "fixture")
	for _, dir := range []string{deep, skipped} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), []byte("name: test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := discoverAgentDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{deep}) {
		t.Fatalf("agent dirs = %v, want %v", got, []string{deep})
	}
}

func TestFileSnapshotsRestoreExistingAndRemoveCreated(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "go.mod")
	created := filepath.Join(root, "go.sum")
	if err := os.WriteFile(existing, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotFiles(existing, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, done := common.NewContext()
	defer done()
	if err := restoreFiles(ctx, snapshots); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original" {
		t.Fatalf("restored contents = %q", contents)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("new file was not removed: %v", err)
	}
}

func TestRunGoHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runGo(ctx, t.TempDir(), "version"); !errors.Is(err, context.Canceled) {
		t.Fatalf("runGo error = %v, want context cancellation", err)
	}
}

func TestNestedGoModDirsExcludesAgentRoot(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base", "code")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, base} {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := nestedGoModDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{base}) {
		t.Fatalf("nested dirs = %v, want %v", got, []string{base})
	}
}

func TestFactoryTemplateDirMapsBaseToTemplate(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "base", "code")
	templateDir := filepath.Join(root, "templates", "factory", "code")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No go.mod.tmpl yet -> not a factory-backed base.
	if got := factoryTemplateDir(root, moduleDir); got != "" {
		t.Fatalf("factoryTemplateDir without template = %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "go.mod.tmpl"), []byte("module {{ .Service.Name.DNSCase }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := factoryTemplateDir(root, moduleDir); got != templateDir {
		t.Fatalf("factoryTemplateDir = %q, want %q", got, templateDir)
	}
	// A module outside base/ has no factory mapping.
	if got := factoryTemplateDir(root, filepath.Join(root, "other")); got != "" {
		t.Fatalf("factoryTemplateDir outside base = %q, want empty", got)
	}
}

func TestRegenerateFactoryTemplateMatchesBase(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "base", "code")
	templateDir := filepath.Join(root, "templates", "factory", "code")
	for _, dir := range []string{moduleDir, templateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	baseMod := "module codefly-base\n\ngo 1.25.12\n\nrequire github.com/codefly-dev/core v0.3.4\n"
	baseSum := "github.com/codefly-dev/core v0.3.4 h1:abc=\ngithub.com/codefly-dev/core v0.3.4/go.mod h1:def=\n"
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(baseMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.sum"), []byte(baseSum), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stale templates still on the old version; only the module line reveals the placeholder token.
	if err := os.WriteFile(filepath.Join(templateDir, "go.mod.tmpl"), []byte("module {{ .Service.Name.DNSCase }}\n\ngo 1.25.12\n\nrequire github.com/codefly-dev/core v0.3.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "go.sum.tmpl"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, done := common.NewContext()
	defer done()
	if err := regenerateFactoryTemplate(ctx, root, moduleDir); err != nil {
		t.Fatal(err)
	}

	gotMod, err := os.ReadFile(filepath.Join(templateDir, "go.mod.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	// The rendered template (placeholder -> base module name) must match base byte-for-byte,
	// which is exactly what the agent's TestFactoryDependencyLocksMatchBase enforces.
	rendered := strings.ReplaceAll(string(gotMod), "{{ .Service.Name.DNSCase }}", "codefly-base")
	if rendered != baseMod {
		t.Fatalf("rendered go.mod.tmpl = %q, want %q", rendered, baseMod)
	}
	gotSum, err := os.ReadFile(filepath.Join(templateDir, "go.sum.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSum) != baseSum {
		t.Fatalf("go.sum.tmpl = %q, want %q", gotSum, baseSum)
	}
}

func TestGoModModuleAndTemplatePlaceholder(t *testing.T) {
	if got := goModModule([]byte("// header\nmodule codefly-base\n\ngo 1.25\n")); got != "codefly-base" {
		t.Fatalf("goModModule = %q", got)
	}
	if got := goModModule([]byte("go 1.25\n")); got != "" {
		t.Fatalf("goModModule without module line = %q, want empty", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod.tmpl")
	if err := os.WriteFile(path, []byte("module {{ .Service.Name.DNSCase }}\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := templateModulePlaceholder(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "{{ .Service.Name.DNSCase }}" {
		t.Fatalf("placeholder = %q", got)
	}
	if _, err := templateModulePlaceholder(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing template did not error")
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
