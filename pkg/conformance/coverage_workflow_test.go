package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// goWorkflow is the part of .github/workflows/go.yml these tests hold.
type goWorkflow struct {
	Jobs map[string]struct {
		Name     string            `yaml:"name"`
		Timeout  string            `yaml:"timeout-minutes"`
		If       string            `yaml:"if"`
		Needs    yaml.Node         `yaml:"needs"`
		Env      map[string]string `yaml:"env"`
		Strategy struct {
			Matrix yaml.Node `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Name string            `yaml:"name"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readGoWorkflow(t *testing.T) goWorkflow {
	t.Helper()
	var workflow goWorkflow
	data := readFile(t, filepath.Join(repositoryRoot(t), ".github/workflows/go.yml"))
	if err := yaml.Unmarshal([]byte(data), &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestQualityGatesKeepTheirBudgets(t *testing.T) {
	job := readGoWorkflow(t).Jobs["quality"]
	if job.Timeout != "${{ matrix.timeout_minutes }}" {
		t.Fatalf("quality budget = %q, want the matrix row to own each gate timeout", job.Timeout)
	}
	var matrix struct {
		Include []struct {
			Gate    string `yaml:"gate"`
			Timeout int    `yaml:"timeout_minutes"`
		} `yaml:"include"`
	}
	if err := job.Strategy.Matrix.Decode(&matrix); err != nil {
		t.Fatal(err)
	}
	wantBudgets := map[string]int{"bootstrap-audit": 8, "control-integration": 12}
	for _, row := range matrix.Include {
		want, ok := wantBudgets[row.Gate]
		if !ok {
			t.Errorf("unexpected quality gate %q carries timeout %d", row.Gate, row.Timeout)
			continue
		}
		if row.Timeout != want {
			t.Errorf("%s timeout = %d, want %d", row.Gate, row.Timeout, want)
		}
		delete(wantBudgets, row.Gate)
	}
	for gate := range wantBudgets {
		t.Errorf("quality gate %s is missing its timeout row", gate)
	}
}

// The suite runs once, in the test shards, under the race detector and with
// coverage; the required `coverage` and `race` contexts stand for it.
func TestTheSuiteRunsOnceUnderRaceAndCoverageWithTimeoutDiagnostics(t *testing.T) {
	job := readGoWorkflow(t).Jobs["test"]
	minutes, err := strconv.Atoi(job.Timeout)
	if err != nil {
		t.Fatalf("test job timeout %q is not a literal number of minutes", job.Timeout)
	}
	jobBudget := time.Duration(minutes) * time.Minute
	for _, step := range job.Steps {
		if step.Name != "Test with race detection and coverage" {
			continue
		}
		args := strings.Fields(step.Run)
		flags := make(map[string]bool, len(args))
		var packageBudget time.Duration
		for _, arg := range args {
			flags[arg] = true
			if value, ok := strings.CutPrefix(arg, "-timeout="); ok {
				if packageBudget, err = time.ParseDuration(value); err != nil {
					t.Fatal(err)
				}
			}
		}
		// The package alarm is what produces a stack trace, so it has to
		// expire while the runner is still listening. The job budget it has
		// to fit inside is spent before the alarm ever starts counting:
		// checkout, toolchain, a cache restore and `go mod download`, then
		// compilation, which reached 2m07s on the cold cache of run
		// 35685320191. The reserve covers all of it, not compilation alone.
		if packageBudget < 3*time.Minute || jobBudget-packageBudget < 4*time.Minute {
			t.Fatalf("test budgets do not fit measured tests and diagnostic headroom: package=%s job=%s", packageBudget, jobBudget)
		}
		for _, flag := range []string{"-v", "-failfast", "-race", "-coverprofile=cover.out", "-covermode=atomic"} {
			if !flags[flag] {
				t.Errorf("the suite's command lost %s", flag)
			}
		}
		if step.Env["CODEFLY_COMPOSITION_OCI_QUALIFY"] != "1" {
			t.Error("the suite must exercise real OCI qualification")
		}
		// A shard whose selection failed or came back empty must fail: go test
		// with no package tests the repository root, which passes.
		if !strings.Contains(step.Run, `selection="$(./scripts/test-shard.sh`) ||
			!strings.Contains(step.Run, `selected nothing to test`) {
			t.Error("the test step no longer fails a shard whose package selection failed or is empty")
		}
		return
	}
	t.Fatal("the test step is missing")
}

// `race` is required, and stands for every shard: it must report whatever the
// shards did (a skipped required check passes) and refuse anything but success.
func TestRaceGateRefusesEveryShardOutcomeButSuccess(t *testing.T) {
	job := readGoWorkflow(t).Jobs["race"]
	if job.Name != "race" || strings.TrimSpace(job.If) != "always()" {
		t.Fatalf("race gate = name %q, if %q; want the required name and always()", job.Name, job.If)
	}
	if job.Needs.Value != "test" {
		t.Fatalf("race gate needs %q, want exactly the test shards", job.Needs.Value)
	}
	if len(job.Steps) != 1 || job.Steps[0].Env["RESULTS"] != "${{ join(needs.*.result, ' ') }}" ||
		!strings.Contains(job.Steps[0].Run, `if [ "$result" != success ]; then`) {
		t.Fatal("race gate no longer refuses every shard result but success")
	}
}

// go.yml runs the suite split by scripts/test-shard.sh. Nothing else notices a
// package or a test that falls between the shards: every shard goes green and
// the tests have silently stopped running. So the partition is proved here,
// against the workflow's own shard count.
func TestTestShardsRunEveryTestOnce(t *testing.T) {
	root := repositoryRoot(t)
	workflow := readGoWorkflow(t)
	job := workflow.Jobs["test"]
	total, err := strconv.Atoi(job.Env["SHARDS"])
	if err != nil {
		t.Fatalf("go.yml's test job must set SHARDS to the matrix size: %v", err)
	}
	// The matrix GitHub expands, not one axis of it: an include: or exclude:
	// beside shard: changes which shards run.
	matrix := job.Strategy.Matrix
	if matrix.Kind != yaml.MappingNode || len(matrix.Content) != 2 || matrix.Content[0].Value != "shard" {
		t.Fatal("the test matrix must be the one literal `shard:` axis, with no include/exclude")
	}
	var shards []int
	if err := matrix.Content[1].Decode(&shards); err != nil {
		t.Fatal(err)
	}
	for i, shard := range shards {
		if shard != i+1 {
			t.Fatalf("the shard matrix is %v; it must be exactly 1..%d", shards, total)
		}
	}
	if len(shards) != total {
		t.Fatalf("the shard matrix has %d shards, SHARDS says %d", len(shards), total)
	}
	if coverage := workflow.Jobs["coverage"]; coverage.Env["SHARDS"] != job.Env["SHARDS"] {
		t.Fatalf("coverage expects %q shard profiles, the test job runs %q", coverage.Env["SHARDS"], job.Env["SHARDS"])
	}

	list := exec.Command("go", "list", "./...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	module := exec.Command("go", "list", "-m")
	module.Dir = root
	modulePath, err := module.Output()
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.TrimSpace(string(modulePath)) + "/"

	whole := map[string]int{}
	patterns := map[string][]*regexp.Regexp{}
	for shard := 1; shard <= total; shard++ {
		cmd := exec.Command("bash", filepath.Join(root, "scripts", "test-shard.sh"), strconv.Itoa(shard), strconv.Itoa(total))
		cmd.Dir = root
		got, err := cmd.Output()
		if err != nil {
			t.Fatalf("shard %d: %v", shard, err)
		}
		var run *regexp.Regexp
		var pkgs []string
		for _, arg := range strings.Fields(string(got)) {
			if value, ok := strings.CutPrefix(arg, "-run="); ok {
				run = regexp.MustCompile(value)
				continue
			}
			pkgs = append(pkgs, arg)
		}
		if len(pkgs) == 0 {
			t.Fatalf("shard %d runs no package", shard)
		}
		if run != nil {
			if len(pkgs) != 1 {
				t.Fatalf("shard %d applies -run to %d packages; a split shard runs one", shard, len(pkgs))
			}
			patterns[pkgs[0]] = append(patterns[pkgs[0]], run)
			continue
		}
		for _, pkg := range pkgs {
			whole[pkg]++
		}
	}

	for _, pkg := range strings.Fields(string(out)) {
		split := patterns[pkg]
		switch {
		case whole[pkg] == 1 && len(split) == 0:
		case whole[pkg] == 0 && len(split) > 0:
			// Every test the package declares, under any build tag, matched
			// by exactly one of its shards' patterns.
			for _, name := range declaredTests(t, filepath.Join(root, strings.TrimPrefix(pkg, prefix))) {
				matched := 0
				for _, run := range split {
					if run.MatchString(name) {
						matched++
					}
				}
				if matched != 1 {
					t.Errorf("%s.%s is run by %d shards, want exactly 1", pkg, name, matched)
				}
			}
		default:
			t.Errorf("%s runs whole on %d shards and split on %d; want exactly one of the two", pkg, whole[pkg], len(split))
		}
		delete(whole, pkg)
		delete(patterns, pkg)
	}
	for pkg := range whole {
		t.Errorf("shards list %s, which go list does not", pkg)
	}
	for pkg := range patterns {
		t.Errorf("shards split %s, which go list does not", pkg)
	}
}

// declaredTests lists the entry points -run selects among in dir: every
// top-level Test, Example and Fuzz function, under any build tag.
func declaredTests(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fset, file, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			name := funcName(decl)
			for _, kind := range []string{"Test", "Example", "Fuzz"} {
				if strings.HasPrefix(name, kind) && name != "TestMain" {
					names = append(names, name)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s declares no tests to split", dir)
	}
	return names
}

func funcName(decl ast.Decl) string {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok || fn.Recv != nil {
		return ""
	}
	return fn.Name.Name
}
