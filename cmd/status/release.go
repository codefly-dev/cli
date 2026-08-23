package status

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/fatih/color"
	"github.com/google/go-github/v89/github"
	"github.com/spf13/cobra"
)

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Check release status of all agents and core",
	Long: `Show release status including:
  - Version comparison between agents and latest core
  - Recent release attempts (successes and failures)
  - Agent health (security audits, tests, manifests)
  - Version deltas`,
	RunE: runRelease,
}

func init() {
	releaseCmd.Flags().Bool("create-issues", false, "auto-create GitHub issues for out-of-date agents")
}

type AgentStatus struct {
	Name       string
	CoreVer    string
	LatestCore string
	Delta      int
	Health     string
	Issues     []string
}

func runRelease(cmd *cobra.Command, args []string) error {
	baseDir := os.Getenv("CODEFLY_DEV_DIR")
	if baseDir == "" {
		baseDir = filepath.Join(os.Getenv("HOME"), "development/deus")
	}

	createIssues, _ := cmd.Flags().GetBool("create-issues")

	fmt.Printf("==> Release Status Report\n\n")

	// Get core version
	coreVer, err := getCoreVersion(filepath.Join(baseDir, "core"))
	if err != nil {
		fmt.Printf("⚠ Could not determine core version: %v\n", err)
		coreVer = "unknown"
	}
	fmt.Printf("Latest Core: %s\n\n", color.GreenString(coreVer))

	// Scan all agents
	fmt.Printf("Agent Versions vs Core:\n")
	fmt.Printf("%-30s %-15s %-15s %s\n", "Agent", "Core Ver", "Delta", "Health")
	fmt.Printf("%s\n", strings.Repeat("-", 90))

	statuses := scanAgents(baseDir, coreVer)
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Delta > statuses[j].Delta
	})

	for _, status := range statuses {
		deltaStr := fmt.Sprintf("%+d", status.Delta)
		if status.Delta > 50 {
			deltaStr = color.RedString(deltaStr)
		} else if status.Delta > 20 {
			deltaStr = color.YellowString(deltaStr)
		} else {
			deltaStr = color.GreenString(deltaStr)
		}

		healthIcon := "✓"
		if len(status.Issues) > 0 {
			healthIcon = color.RedString("✗")
		}

		fmt.Printf("%-30s %-15s %-15s %s\n",
			status.Name, status.CoreVer, deltaStr, healthIcon)

		if len(status.Issues) > 0 {
			for _, issue := range status.Issues {
				fmt.Printf("  └─ %s\n", color.RedString(issue))
			}
		}
	}

	fmt.Printf("\n==> Summary\n")
	critical := 0
	for _, s := range statuses {
		if s.Delta > 50 {
			critical++
		}
	}
	fmt.Printf("Critical (>50 versions behind): %d\n", critical)
	fmt.Printf("Total agents scanned: %d\n", len(statuses))

	if createIssues {
		fmt.Printf("\n==> Creating GitHub issues for agents with issues...\n")
		issuesCreated := 0
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		for _, s := range statuses {
			if s.Delta > 50 || len(s.Issues) > 0 {
				// Never file a chore issue against an archived repo: it's frozen
				// and can't be bumped or released, so the issue would be noise
				// nobody can act on. Resolve the repo from the clone's origin so
				// this works regardless of org; an unresolvable remote falls
				// through to the normal (non-archived) path.
				if repoIsArchived(ctx, filepath.Join(baseDir, s.Name)) {
					fmt.Printf("⏭ Skipping archived repo %s\n", s.Name)
					continue
				}
				if err := createAgentIssue(baseDir, s); err != nil {
					fmt.Printf("⚠ Failed to create issue for %s: %v\n", s.Name, err)
				} else {
					fmt.Printf("✓ Created issue for %s\n", s.Name)
					issuesCreated++
				}
			}
		}
		fmt.Printf("\n✓ Created %d issues\n", issuesCreated)
	}

	return nil
}

func scanAgents(baseDir, latestCore string) []AgentStatus {
	var statuses []AgentStatus

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return statuses
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "service-") && !strings.HasPrefix(name, "module-") {
			continue
		}

		agentPath := filepath.Join(baseDir, name)
		gomod := filepath.Join(agentPath, "go.mod")

		if _, err := os.Stat(gomod); err != nil {
			continue
		}

		coreVer := getAgentCoreVersion(gomod)
		status := AgentStatus{
			Name:       name,
			CoreVer:    coreVer,
			LatestCore: latestCore,
			Health:     "unknown",
		}

		// Calculate delta
		status.Delta = versionDelta(latestCore, coreVer)

		// Check for issues
		status.Issues = checkAgentHealth(agentPath)
		if len(status.Issues) == 0 {
			status.Health = "healthy"
		} else {
			status.Health = "⚠ issues"
		}

		statuses = append(statuses, status)
	}

	return statuses
}

func getCoreVersion(corePath string) (string, error) {
	ver, err := readVersionFromGoMod(filepath.Join(corePath, "go.mod"), "github.com/codefly-dev/core")
	if err != nil {
		return "", err
	}
	return ver, nil
}

func getAgentCoreVersion(gomodPath string) string {
	ver, _ := readVersionFromGoMod(gomodPath, "github.com/codefly-dev/core")
	return ver
}

func readVersionFromGoMod(gomodPath, module string) (string, error) {
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		return "unknown", err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, module) {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1], nil
			}
		}
	}
	return "unknown", nil
}

func versionDelta(v1, v2 string) int {
	// Simple delta: extract major.minor.patch numbers
	v1Num := extractVersionNumber(v1)
	v2Num := extractVersionNumber(v2)
	return v1Num - v2Num
}

func extractVersionNumber(v string) int {
	// Convert v0.2.128 → 128 (just use patch version for simplicity)
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) >= 3 {
		var num int
		fmt.Sscanf(parts[2], "%d", &num)
		return num
	}
	return 0
}

func checkAgentHealth(agentPath string) []string {
	var issues []string

	// Check manifest
	agentYaml := filepath.Join(agentPath, "agent.codefly.yaml")
	if _, err := os.Stat(agentYaml); err != nil {
		// Try alternative paths
		agentYaml = filepath.Join(agentPath, "module.codefly.yaml")
		if _, err := os.Stat(agentYaml); err != nil {
			return issues
		}
	}

	data, _ := os.ReadFile(agentYaml)
	content := string(data)

	if !strings.Contains(content, "publisher:") {
		issues = append(issues, "missing publisher in manifest")
	}
	if !strings.Contains(content, "kind:") {
		issues = append(issues, "missing kind in manifest")
	}
	if !strings.Contains(content, "name:") {
		issues = append(issues, "missing name in manifest")
	}
	if !strings.Contains(content, "version:") {
		issues = append(issues, "missing version in manifest")
	}

	return issues
}

// repoIsArchived reports whether the clone at path points at an archived GitHub
// repo. A remote we can't resolve (not a git repo, no origin, non-GitHub) is
// treated as non-archived so the caller proceeds exactly as before.
func repoIsArchived(ctx context.Context, path string) bool {
	owner, repo, err := agentRepository(path)
	if err != nil {
		return false
	}
	return gh.Archived(ctx, owner, repo)
}

func createAgentIssue(baseDir string, status AgentStatus) error {
	agentPath := filepath.Join(baseDir, status.Name)

	title := fmt.Sprintf("chore: update core to %s (currently %s, %d versions behind)",
		status.LatestCore, status.CoreVer, status.Delta)

	body := fmt.Sprintf("## Update core dependency\n\nThis agent is %d versions behind the latest core (%s → %s).\n\n### Changes needed\n- [ ] Update core from %s to %s\n- [ ] Run tests to verify compatibility\n- [ ] Create release\n\n### Status\n- Current core: %s\n- Latest core: %s\n- Delta: %d versions\n\nRun:\n```bash\ncd %s\ngo get -u github.com/codefly-dev/core@latest\ngo mod tidy\n```\n\nThen test and release.",
		status.Delta, status.CoreVer, status.LatestCore,
		status.CoreVer, status.LatestCore,
		status.CoreVer, status.LatestCore, status.Delta,
		agentPath)

	owner, repo, err := agentRepository(agentPath)
	if err != nil {
		return err
	}
	client, err := gh.NewClient()
	if err != nil {
		return err
	}
	_, _, err = client.Issues.Create(context.Background(), owner, repo, &github.IssueRequest{
		Title:  github.Ptr(title),
		Body:   github.Ptr(body),
		Labels: &[]string{"chore", "dependencies"},
	})
	return err
}

func agentRepository(agentPath string) (string, string, error) {
	//nolint:gosec // git is invoked with fixed subcommands; agentPath is a scanned filesystem path, never a shell.
	out, err := exec.Command("git", "-C", agentPath, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve %s origin remote: %w", agentPath, err)
	}
	return parseGitHubRemote(strings.TrimSpace(string(out)))
}

func parseGitHubRemote(remote string) (string, string, error) {
	trimmed := strings.TrimSuffix(remote, ".git")
	switch {
	case strings.HasPrefix(trimmed, "git@github.com:"):
		trimmed = strings.TrimPrefix(trimmed, "git@github.com:")
	case strings.HasPrefix(trimmed, "https://github.com/"):
		trimmed = strings.TrimPrefix(trimmed, "https://github.com/")
	default:
		return "", "", fmt.Errorf("unrecognized GitHub remote %q", remote)
	}
	owner, repo, ok := strings.Cut(trimmed, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("unrecognized GitHub remote %q", remote)
	}
	return owner, repo, nil
}
