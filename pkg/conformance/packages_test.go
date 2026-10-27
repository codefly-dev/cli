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
	if compile := "go test -tags " + row.BuildTag + " -c"; lines[0] != compile {
		t.Errorf("%s compiles with %q, want %q for row %s", script, lines[0], compile, id)
	}
	if got := lines[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("%s runs packages %v, want the row's gates and tagged packages %v", script, got, want)
	}
	// A literal equal to today's derived value prints the same list, so the
	// script must not spell any of them: a hard-coded tag or package would
	// stop following the matrix the day it changes.
	body := readFile(t, filepath.Join(root, script))
	for _, literal := range append([]string{row.BuildTag}, want...) {
		if strings.Contains(body, literal) {
			t.Errorf("%s spells %q itself instead of deriving it from the matrix", script, literal)
		}
	}
	// What --list printed is what compiles: the script builds a test binary
	// only through the printed command, never through a second go test line.
	for _, line := range strings.Split(body, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") || !strings.Contains(code, "go test") {
			continue
		}
		if code != "compile=(go test)" {
			t.Errorf("%s runs go test outside the command --list prints: %q", script, code)
		}
	}
	if !strings.Contains(body, `"${compile[@]}" -o`) {
		t.Errorf("%s does not compile through the command --list prints", script)
	}
}

// TestDocsOnlyScopeSeesBothSidesOfARename holds the classifier that lets
// docker-generate pass without its shards. Git's rename detection would list
// only a moved file's new path, so moving code or a fixture into docs/ or to a
// .md name must still count as a change the row has to run for.
func TestDocsOnlyScopeSeesBothSidesOfARename(t *testing.T) {
	script := filepath.Join(repositoryRoot(t), "scripts", "generate-qualification-scope.sh")
	const source = "package generate\n\nfunc Generate() string { return \"a long enough body for rename detection\" }\n"
	cases := []struct {
		name   string
		change func(t *testing.T, dir string)
		want   string
	}{
		{"prose only", func(t *testing.T, dir string) {
			writeSource(t, dir, "docs/guide.md", "# Guide, revised\n")
			writeSource(t, dir, "README.md", "# Readme, revised\n")
		}, "relevant=false"},
		{"code moved into docs", func(t *testing.T, dir string) {
			gitIn(t, dir, "mv", "cmd/generate/generate.go", "docs/generate.go")
		}, "relevant=true"},
		{"test renamed to markdown", func(t *testing.T, dir string) {
			gitIn(t, dir, "mv", "cmd/generate/generate_test.go", "cmd/generate/generate_test.md")
		}, "relevant=true"},
		{"fixture moved into docs", func(t *testing.T, dir string) {
			gitIn(t, dir, "mv", "cmd/sync/testdata/fixture.md", "docs/fixture.md")
		}, "relevant=true"},
		{"markdown fixture edited", func(t *testing.T, dir string) {
			writeSource(t, dir, "cmd/sync/testdata/fixture.md", "# Fixture, revised\n")
		}, "relevant=true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitIn(t, dir, "init", "-q")
			writeSource(t, dir, "cmd/generate/generate.go", source)
			writeSource(t, dir, "cmd/generate/generate_test.go", strings.Replace(source, "func Generate", "func Test", 1))
			writeSource(t, dir, "cmd/sync/testdata/fixture.md", "# Fixture\n\nA fixture long enough for rename detection.\n")
			writeSource(t, dir, "docs/guide.md", "# Guide\n")
			writeSource(t, dir, "README.md", "# Readme\n")
			gitIn(t, dir, "add", "-A")
			gitIn(t, dir, "commit", "-q", "-m", "base")
			tc.change(t, dir)
			gitIn(t, dir, "add", "-A")
			gitIn(t, dir, "commit", "-q", "-m", "change")

			cmd := exec.Command("bash", script, "HEAD~1")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", script, err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("scope = %s, want %s", got, tc.want)
			}
		})
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
