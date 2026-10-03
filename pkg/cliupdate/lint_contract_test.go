package cliupdate

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type goWorkflow struct {
	Jobs map[string]goWorkflowJob `yaml:"jobs"`
}

type goWorkflowJob struct {
	If         string `yaml:"if"`
	TimeoutMin string `yaml:"timeout-minutes"`
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
	Run  string `yaml:"run"`
	With struct {
		Version       string `yaml:"version"`
		OnlyNewIssues bool   `yaml:"only-new-issues"`
		Args          string `yaml:"args"`
		FetchDepth    int    `yaml:"fetch-depth"`
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

func checkoutStep(job goWorkflowJob) (goWorkflowStep, bool) {
	for _, step := range job.Steps {
		if regexp.MustCompile(`^actions/checkout`).MatchString(step.Uses) {
			return step, true
		}
	}
	return goWorkflowStep{}, false
}

// checkoutStepText returns the lint job's text so an explicit "fetch-depth: 0"
// can be told from the zero value an omitted key decodes to.
func checkoutStepText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(repositoryPath(".github/workflows/go.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "\n  lint:\n")
	if start < 0 {
		t.Fatal("lint job not found in go.yml")
	}
	return text[start:]
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
// gate. Its own budget must give the analysis timeout headroom to finish and
// report cleanly, independently of the quality matrix's per-gate budgets.
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
	lintTimeout, err := strconv.Atoi(lint.TimeoutMin)
	if err != nil || lintTimeout < 10 {
		t.Fatalf("lint timeout = %q, want at least ten minutes for cold-cache analysis", lint.TimeoutMin)
	}

	var config golangciConfig
	readRepositoryYAML(t, ".golangci.yaml", &config)
	analysisTimeout, err := time.ParseDuration(config.Run.Timeout)
	if err != nil {
		t.Fatalf("run.timeout %q is not a duration: %v", config.Run.Timeout, err)
	}
	jobBudget := time.Duration(lintTimeout) * time.Minute
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
	// New findings only, with a baseline that holds for any pull request:
	// golangci-lint's own merge-base against main over a full checkout. The
	// action's only-new-issues fetches the pull request's patch through the
	// GitHub API and, past the API's 20,000-line cap, gets no patch and
	// reports every issue in the repository as new — a large PR went red on
	// findings in files it never touched. That mode stays off by name.
	if step.With.OnlyNewIssues {
		t.Fatal("golangci-lint step must not rely on only-new-issues: the action's PR patch is refused past the API's 20,000-line cap, and every issue then reads as new")
	}
	if !strings.Contains(step.With.Args, "--new-from-merge-base=origin/main") {
		t.Fatalf("golangci-lint step args %q must gate on new findings against the merge-base with main", step.With.Args)
	}
	checkout, found := checkoutStep(workflow.Jobs["lint"])
	if !found {
		t.Fatal("lint job has no checkout step")
	}
	if checkout.With.FetchDepth != 0 || !strings.Contains(checkoutStepText(t), "fetch-depth: 0") {
		t.Fatal("the lint job's checkout must fetch the full history (fetch-depth: 0), or the merge-base with main cannot be computed")
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
