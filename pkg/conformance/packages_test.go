package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeSource(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPackagesUnionGatesAndTaggedSources(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "cmd/tagged/a_test.go", "// Copyright.\n\n//go:build linux && companion\n\npackage tagged\n")
	writeSource(t, root, "cmd/negated/b.go", "//go:build !companion\n\npackage negated\n")
	writeSource(t, root, "cmd/other/c.go", "//go:build integration\n\npackage other\n")
	writeSource(t, root, "cmd/late/d.go", "package late\n\n//go:build companion\n")
	writeSource(t, root, "cmd/fixture/testdata/e.go", "//go:build companion\n\npackage fixture\n")
	writeSource(t, root, "web/node_modules/f/f.go", "//go:build companion\n\npackage f\n")

	row := Row{ID: "row", Gates: []string{"cmd/gated", "cmd/tagged"}, BuildTag: "companion"}
	got, err := row.Packages(root)
	if err != nil {
		t.Fatal(err)
	}
	// A negated constraint still names the tag, so its package is compiled
	// too; a constraint after the package clause is no constraint at all.
	want := []string{"cmd/gated", "cmd/negated", "cmd/tagged"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Packages = %v, want %v", got, want)
	}

	if _, err := (&Row{ID: "empty", BuildTag: "absent"}).Packages(root); err == nil {
		t.Fatal("a row with no gate and no tagged file derived packages; nothing would run")
	}
}

// TestGenerateQualificationScriptRunsTheMatrixRow proves the Docker generate
// job, the matrix row it claims and the build-tagged sources agree: the script
// compiles every gate package and every package with a file built under the
// row's tag, under that tag, with no list of its own to drift. The expected
// set is computed here independently of Row.Packages.
func TestGenerateQualificationScriptRunsTheMatrixRow(t *testing.T) {
	const workflow, script = "generate-qualification.yml", "scripts/test-generate-isolated.sh"
	root := repositoryRoot(t)
	var document struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, ".github", "workflows", workflow))), &document); err != nil {
		t.Fatal(err)
	}
	id := document.Env[RequiredEnv]
	row, ok := Default().Row(id)
	if !ok {
		t.Fatalf("%s claims row %q in its workflow env, which the matrix does not have", workflow, id)
	}
	if !slices.Contains(row.CI, workflow+"#generate") {
		t.Fatalf("row %s does not name %s#generate in its ci list %v", id, workflow, row.CI)
	}
	runsScript := false
	for _, step := range document.Jobs["generate"].Steps {
		runsScript = runsScript || strings.Contains(step.Run, "./"+script)
	}
	if !runsScript {
		t.Fatalf("the generate job of %s never runs %s", workflow, script)
	}
	if row.BuildTag == "" {
		t.Fatalf("row %s declares no build_tag, so its tagged tests could never be compiled", id)
	}

	expected := map[string]bool{}
	for _, gate := range row.Gates {
		expected[gate] = true
	}
	tagged := regexp.MustCompile(`(?m)^//go:build .*\b` + regexp.QuoteMeta(row.BuildTag) + `\b`)
	taggedFiles := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || !tagged.MatchString(readFile(t, path)) {
			return nil
		}
		taggedFiles++
		pkg, relErr := filepath.Rel(root, filepath.Dir(path))
		expected[filepath.ToSlash(pkg)] = true
		return relErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if taggedFiles == 0 {
		t.Fatalf("no file carries build tag %s; the row's tag proves nothing", row.BuildTag)
	}
	want := make([]string, 0, len(expected))
	for pkg := range expected {
		want = append(want, pkg)
	}
	sort.Strings(want)

	cmd := exec.Command("bash", script, "--list")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), RequiredEnv+"="+id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s --list: %v\n%s", script, err, out)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if lines[0] != row.BuildTag {
		t.Errorf("%s compiles under tag %q, row %s declares %q", script, lines[0], id, row.BuildTag)
	}
	if got := lines[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("%s runs packages %v, want the row's gates and tagged packages %v", script, got, want)
	}
}
