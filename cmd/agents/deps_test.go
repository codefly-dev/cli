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

func TestTemplateLockPinsOtherCore(t *testing.T) {
	const core = "github.com/codefly-dev/core"
	for _, tc := range []struct {
		name    string
		content string
		want    string
		stale   bool
	}{
		{
			name:    "go.mod require at wanted version",
			content: "require (\n\tgithub.com/codefly-dev/core v0.3.4\n)\n",
			want:    "v0.3.4",
			stale:   false,
		},
		{
			name:    "go.mod require at older version",
			content: "require (\n\tgithub.com/codefly-dev/core v0.3.2\n)\n",
			want:    "v0.3.4",
			stale:   true,
		},
		{
			name:    "go.sum hash lines at wanted version",
			content: "github.com/codefly-dev/core v0.3.4 h1:abc\ngithub.com/codefly-dev/core v0.3.4/go.mod h1:def\n",
			want:    "v0.3.4",
			stale:   false,
		},
		{
			name:    "go.sum go.mod hash line at older version",
			content: "github.com/codefly-dev/core v0.3.2/go.mod h1:def\n",
			want:    "v0.3.4",
			stale:   true,
		},
		{
			name:    "single-line require at older version",
			content: "require github.com/codefly-dev/core v0.3.2\n",
			want:    "v0.3.4",
			stale:   true,
		},
		{
			name:    "single-line require at wanted version",
			content: "require github.com/codefly-dev/core v0.3.4 // indirect\n",
			want:    "v0.3.4",
			stale:   false,
		},
		{
			name:    "replace directive is not treated as a version pin",
			content: "replace github.com/codefly-dev/core => ../core\n",
			want:    "v0.3.4",
			stale:   false,
		},
		{
			name:    "unrelated module at a different version is ignored",
			content: "require (\n\tgithub.com/codefly-dev/sdk-go v0.1.0\n)\n",
			want:    "v0.3.4",
			stale:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := templateLockPinsOtherCore(tc.content, core, tc.want); got != tc.stale {
				t.Fatalf("templateLockPinsOtherCore = %v, want %v", got, tc.stale)
			}
		})
	}
}

func TestGoModVersionHandlesRequireForms(t *testing.T) {
	const core = "github.com/codefly-dev/core"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "block require",
			body: "module x\n\nrequire (\n\tgithub.com/codefly-dev/core v0.3.4\n)\n",
			want: "v0.3.4",
		},
		{
			name: "single-line require",
			body: "module x\n\nrequire github.com/codefly-dev/core v0.3.4\n",
			want: "v0.3.4",
		},
		{
			name: "core absent",
			body: "module x\n\nrequire github.com/codefly-dev/sdk-go v0.1.0\n",
			want: "?",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := goModVersion(dir, core); got != tc.want {
				t.Fatalf("goModVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStaleCoreTemplateLocksFindsDriftAndSkipsGeneratedTrees(t *testing.T) {
	const core = "github.com/codefly-dev/core"
	root := t.TempDir()

	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A stale factory template pair (still on the old core) and a current one.
	write("templates/factory/code/go.mod.tmpl", "require (\n\tgithub.com/codefly-dev/core v0.3.2\n)\n")
	write("templates/factory/code/go.sum.tmpl", "github.com/codefly-dev/core v0.3.2/go.mod h1:old\n")
	write("templates/other/code/go.mod.tmpl", "require (\n\tgithub.com/codefly-dev/core v0.3.4\n)\n")
	// Real go.mod files and non-template files must be ignored.
	write("base/code/go.mod", "module codefly-base\n\nrequire github.com/codefly-dev/core v0.3.2\n")
	// Anything under a skipped tree must not be reported.
	write("testdata/fixture/go.mod.tmpl", "require (\n\tgithub.com/codefly-dev/core v0.3.2\n)\n")

	stale, err := staleCoreTemplateLocks(root, core, "v0.3.4")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(root, "templates/factory/code/go.mod.tmpl"),
		filepath.Join(root, "templates/factory/code/go.sum.tmpl"),
	}
	if !reflect.DeepEqual(stale, want) {
		t.Fatalf("stale template locks = %v, want %v", stale, want)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
