package cliupdate

import (
	"os"
	"regexp"
	"slices"
	"testing"
	"time"
)

type goWorkflow struct {
	Jobs map[string]goWorkflowJob `yaml:"jobs"`
}

type goWorkflowJob struct {
	If         string `yaml:"if"`
	TimeoutMin int    `yaml:"timeout-minutes"`
	Strategy   struct {
		Matrix struct {
			Gate []string `yaml:"gate"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []goWorkflowStep `yaml:"steps"`
}

type goWorkflowStep struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	With struct {
		Version       string `yaml:"version"`
		OnlyNewIssues bool   `yaml:"only-new-issues"`
	} `yaml:"with"`
}

type golangciConfig struct {
	Version string `yaml:"version"`
	Run     struct {
		Timeout string `yaml:"timeout"`
	} `yaml:"run"`
	Linters struct {
		Exclusions golangciExclusions `yaml:"exclusions"`
	} `yaml:"linters"`
	Formatters struct {
		Exclusions golangciExclusions `yaml:"exclusions"`
	} `yaml:"formatters"`
}

type golangciExclusions struct {
	Paths []string `yaml:"paths"`
}

func lintStep(job goWorkflowJob) (goWorkflowStep, bool) {
	for _, step := range job.Steps {
		if regexp.MustCompile(`^golangci/golangci-lint-action`).MatchString(step.Uses) {
			return step, true
		}
	}
	return goWorkflowStep{}, false
}

// The linter that could not load the pre-v2 config must now load: an explicit
// v2 schema, and the protobuf/test exclusions preserved for both the linters
// and the formatters (the migrator dropped them from both).
func TestGolangciConfigIsV2AndPreservesGeneratedExclusions(t *testing.T) {
	var config golangciConfig
	readRepositoryYAML(t, ".golangci.yaml", &config)

	if config.Version != "2" {
		t.Fatalf("config version = %q, want \"2\"", config.Version)
	}

	preserved := []string{`.*\.pb\.go$`, `.*\.pb\.gw\.go$`, `.*_test\.go$`}
	for _, section := range []struct {
		name  string
		paths []string
	}{
		{"linters", config.Linters.Exclusions.Paths},
		{"formatters", config.Formatters.Exclusions.Paths},
	} {
		for _, want := range preserved {
			if !slices.Contains(section.paths, want) {
				t.Errorf("%s exclusions do not preserve %q; got %v", section.name, want, section.paths)
			}
		}
	}
}

// only-new-issues has a defined baseline only against a pull-request base, so
// the lint job must be pull-request scoped and never a push-triggered quality
// gate. Its budget must exceed the 5-minute quality gates and give the
// analysis timeout headroom to finish and report cleanly.
func TestLintJobIsPullRequestScopedWithHeadroom(t *testing.T) {
	var workflow goWorkflow
	readRepositoryYAML(t, ".github/workflows/go.yml", &workflow)

	quality, found := workflow.Jobs["quality"]
	if !found {
		t.Fatal("quality job is missing")
	}
	if slices.Contains(quality.Strategy.Matrix.Gate, "lint") {
		t.Fatal("lint must not be a quality gate: those run on push, where only-new-issues has no baseline")
	}

	lint, found := workflow.Jobs["lint"]
	if !found {
		t.Fatal("lint job is missing")
	}
	if lint.If != "github.event_name == 'pull_request'" {
		t.Fatalf("lint job condition = %q, want it scoped to pull_request", lint.If)
	}
	if lint.TimeoutMin <= quality.TimeoutMin {
		t.Fatalf("lint timeout = %dm, want more headroom than the quality gates (%dm)", lint.TimeoutMin, quality.TimeoutMin)
	}

	var config golangciConfig
	readRepositoryYAML(t, ".golangci.yaml", &config)
	analysisTimeout, err := time.ParseDuration(config.Run.Timeout)
	if err != nil {
		t.Fatalf("run.timeout %q is not a duration: %v", config.Run.Timeout, err)
	}
	jobBudget := time.Duration(lint.TimeoutMin) * time.Minute
	if analysisTimeout >= jobBudget {
		t.Fatalf("analysis timeout %s must be strictly under the lint job budget %s so an overrun reports as a linter timeout, not a hard job kill", analysisTimeout, jobBudget)
	}
	if analysisTimeout <= 5*time.Minute {
		t.Fatalf("analysis timeout %s leaves no cold-cache headroom over the 5m quality budget", analysisTimeout)
	}
}

// The CI linter version and the version the Makefile installs for `make lint`
// must not drift, so a local run reproduces CI.
func TestLintVersionPinnedConsistently(t *testing.T) {
	var workflow goWorkflow
	readRepositoryYAML(t, ".github/workflows/go.yml", &workflow)

	step, found := lintStep(workflow.Jobs["lint"])
	if !found {
		t.Fatal("lint job has no golangci-lint-action step")
	}
	if !step.With.OnlyNewIssues {
		t.Fatal("golangci-lint step must set only-new-issues to gate on new findings only")
	}

	makefile, err := os.ReadFile(repositoryPath("Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`GOLANGCI_LINT_VERSION\s*\?=\s*(\S+)`).FindSubmatch(makefile)
	if match == nil {
		t.Fatal("Makefile does not pin GOLANGCI_LINT_VERSION")
	}
	if makefileVersion := string(match[1]); makefileVersion != step.With.Version {
		t.Fatalf("Makefile pins golangci-lint %s but CI pins %s", makefileVersion, step.With.Version)
	}
}
