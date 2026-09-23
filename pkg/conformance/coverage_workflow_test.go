package conformance

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCoverageBudgetPreservesQualificationAndTimeoutDiagnostics(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Timeout  string `yaml:"timeout-minutes"`
			Strategy struct {
				Matrix struct {
					Include []struct {
						Gate    string `yaml:"gate"`
						Timeout int    `yaml:"timeout_minutes"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	data := readFile(t, filepath.Join(repositoryRoot(t), ".github/workflows/go.yml"))
	if err := yaml.Unmarshal([]byte(data), &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["quality"]
	if job.Timeout != "${{ matrix.timeout_minutes }}" {
		t.Fatalf("quality budget = %q, want the matrix row to own each gate timeout", job.Timeout)
	}
	wantBudgets := map[string]int{
		"bootstrap-audit":     8,
		"control-integration": 12,
		"coverage":            16,
		"race":                20,
	}
	var coverageMinutes int
	for _, row := range job.Strategy.Matrix.Include {
		want, ok := wantBudgets[row.Gate]
		if !ok {
			t.Errorf("unexpected quality gate %q carries timeout %d", row.Gate, row.Timeout)
			continue
		}
		if row.Timeout != want {
			t.Errorf("%s timeout = %d, want %d", row.Gate, row.Timeout, want)
		}
		if row.Gate == "coverage" {
			coverageMinutes = row.Timeout
		}
		delete(wantBudgets, row.Gate)
	}
	for gate := range wantBudgets {
		t.Errorf("quality gate %s is missing its timeout row", gate)
	}
	if coverageMinutes == 0 {
		t.Fatal("coverage gate timeout is missing")
	}
	jobBudget := time.Duration(coverageMinutes) * time.Minute
	for _, step := range job.Steps {
		if step.Name != "Test with coverage" {
			continue
		}
		args := strings.Fields(step.Run)
		flags := make(map[string]bool, len(args))
		var packageBudget time.Duration
		for _, arg := range args {
			flags[arg] = true
			if value, ok := strings.CutPrefix(arg, "-timeout="); ok {
				var err error
				packageBudget, err = time.ParseDuration(value)
				if err != nil {
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
			t.Fatalf("coverage budgets do not fit measured tests and diagnostic headroom: package=%s job=%s", packageBudget, jobBudget)
		}
		for _, flag := range []string{"./...", "-v", "-failfast", "-coverprofile=cover.out", "-covermode=atomic", "-coverpkg=./..."} {
			if !flags[flag] {
				t.Errorf("coverage command lost %s", flag)
			}
		}
		if step.Env["CODEFLY_COMPOSITION_OCI_QUALIFY"] != "1" {
			t.Error("coverage must exercise real OCI qualification")
		}
		return
	}
	t.Fatal("coverage test step is missing")
}
