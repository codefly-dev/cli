package self

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath resolves scripts/bootstrap.sh relative to this test file so the
// test runs the real script, not a copy.
func scriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "scripts", "bootstrap.sh")
}

// stubBin writes fake `git` and `gh` executables into a fresh bin dir and
// returns a PATH value with that dir first. The git stub records clone
// destinations by creating <dest>/.git (so re-runs see the repo as existing)
// and fails any repo whose name contains "broken"; the gh stub emits a fixed
// org listing as name<TAB>topics tsv, standing in for the jq-filtered API call.
func stubBin(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()

	gitStub := `#!/usr/bin/env bash
if [ "$1" = "clone" ]; then
  dest="${@: -1}"
  case "$dest" in
    *broken*) echo "fatal: repository not found" >&2; exit 1;;
  esac
  mkdir -p "$dest/.git"
fi
exit 0
`
	ghStub := `#!/usr/bin/env bash
printf '%s\t%s\n' core ''
printf '%s\t%s\n' llm ''
printf '%s\t%s\n' cli ''
printf '%s\t%s\n' service-go 'codefly-service'
printf '%s\t%s\n' agent-go 'codefly-agent'
printf '%s\t%s\n' broken-repo ''
`
	write := func(name, body string) {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	write("git", gitStub)
	write("gh", ghStub)
	return bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

func runBootstrap(t *testing.T, root, path string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t), "--dir", root}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBootstrapClonesDiscoveredReposAndSkipsExisting(t *testing.T) {
	root := t.TempDir()
	path := stubBin(t)

	// Pre-existing checkout must be left untouched and reported as skipped.
	if err := os.MkdirAll(filepath.Join(root, "core", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// broken-repo makes clone fail; the script must exit non-zero but still
	// clone the healthy repos.
	out, err := runBootstrap(t, root, path)
	if err == nil {
		t.Fatalf("expected non-zero exit due to broken-repo, got success\n%s", out)
	}

	for _, name := range []string{"llm", "cli", "service-go", "agent-go"} {
		if _, statErr := os.Stat(filepath.Join(root, name, ".git")); statErr != nil {
			t.Errorf("expected %s cloned, but %s/.git missing\n%s", name, name, out)
		}
	}
	if !strings.Contains(out, "core") || !strings.Contains(out, "skipped (already exists)") {
		t.Errorf("expected core reported as skipped\n%s", out)
	}
	if !strings.Contains(out, "Done: 4 cloned, 1 skipped, 1 failed") {
		t.Errorf("unexpected summary line\n%s", out)
	}
}

func TestBootstrapDryRunClonesNothing(t *testing.T) {
	root := t.TempDir()
	path := stubBin(t)

	out, err := runBootstrap(t, root, path, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run should succeed: %v\n%s", err, out)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("dry-run created directories in root: %v\n%s", entries, out)
	}
	if !strings.Contains(out, "Dry run:") {
		t.Errorf("expected dry-run summary\n%s", out)
	}
}

func TestBootstrapAgentsFilterUsesTopics(t *testing.T) {
	root := t.TempDir()
	path := stubBin(t)

	out, err := runBootstrap(t, root, path, "--agents")
	if err != nil {
		t.Fatalf("agents run failed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(root, "agent-go", ".git")); statErr != nil {
		t.Errorf("expected agent-go cloned with --agents\n%s", out)
	}
	for _, name := range []string{"core", "llm", "cli", "service-go"} {
		if _, statErr := os.Stat(filepath.Join(root, name)); statErr == nil {
			t.Errorf("--agents should not clone %s\n%s", name, out)
		}
	}
}

func TestBootstrapExplicitNamesSkipDiscovery(t *testing.T) {
	root := t.TempDir()
	// No gh stub reachable: explicit names must not invoke discovery. Provide
	// only a git stub on PATH.
	bin := t.TempDir()
	gitStub := "#!/usr/bin/env bash\n[ \"$1\" = clone ] && mkdir -p \"${@: -1}/.git\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitStub), 0o755); err != nil {
		t.Fatal(err)
	}
	path := bin + string(os.PathListSeparator) + os.Getenv("PATH")

	out, err := runBootstrap(t, root, path, "core", "llm")
	if err != nil {
		t.Fatalf("explicit-names run failed: %v\n%s", err, out)
	}
	for _, name := range []string{"core", "llm"} {
		if _, statErr := os.Stat(filepath.Join(root, name, ".git")); statErr != nil {
			t.Errorf("expected %s cloned\n%s", name, out)
		}
	}
	if !strings.Contains(out, "Done: 2 cloned") {
		t.Errorf("unexpected summary\n%s", out)
	}
}
