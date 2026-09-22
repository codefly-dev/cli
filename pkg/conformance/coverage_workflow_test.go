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
		t.Fatal("quality job must use each gate's measured budget")
	}
	var jobBudget time.Duration
	for _, row := range job.Strategy.Matrix.Include {
		if row.Gate == "coverage" {
			jobBudget = time.Duration(row.Timeout) * time.Minute
		} else if row.Timeout != 8 {
			t.Errorf("coverage adjustment changed %s's budget", row.Gate)
		}
	}
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
		// The observed four-minute cumulative alarm is not a per-test timeout.
		if packageBudget < 7*time.Minute || jobBudget < packageBudget+5*time.Minute || jobBudget > 15*time.Minute {
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
