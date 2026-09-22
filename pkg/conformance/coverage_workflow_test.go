package conformance

import (
	"path/filepath"
	"strconv"
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
	minutes, err := strconv.Atoi(job.Timeout)
	if err != nil || minutes != 8 {
		t.Fatalf("quality budget = %q, want every gate to share eight minutes", job.Timeout)
	}
	jobBudget := time.Duration(minutes) * time.Minute
	for _, row := range job.Strategy.Matrix.Include {
		if row.Timeout != 0 {
			t.Errorf("%s carries a budget of its own; a slow gate is diagnosed, not given more time", row.Gate)
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
		// The package alarm is what produces a stack trace, so it has to
		// expire while the runner is still listening — and still leave room
		// for compilation and the gates that run after the suite.
		if packageBudget < 3*time.Minute || jobBudget-packageBudget < 3*time.Minute {
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
