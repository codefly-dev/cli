package agents

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services/audit"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type agentYAML struct {
	Publisher string `yaml:"publisher"`
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`

	// Quarantine marks an agent as NOT ready for the new-style architecture.
	// `agent build --all` (and `self build --with-agents`) SKIP quarantined
	// agents so a bulk rebuild only touches agents that have been migrated —
	// you don't ship half-converted plugins. An explicit single build
	// (`agent build --dir X`) still builds it, with a warning, so you can
	// iterate on the fix. QuarantineReason is shown in the skip/warn line.
	Quarantine       bool   `yaml:"quarantine"`
	QuarantineReason string `yaml:"quarantine_reason"`
}

// BuildCmd builds an agent binary from source and installs it locally.
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build an agent binary from source and install it locally",
	Long: `Build compiles the agent in the current (or specified) directory and
installs the binary to ~/.codefly/agents/ so it can be loaded by the
Gateway daemon.

The directory must contain an agent.codefly.yaml with publisher, kind,
name, and version fields.

When run inside the codefly.dev monorepo, local replace directives for
wool and core are added automatically so go mod tidy succeeds without
requiring published module versions.

Use --all from the agents/services/ directory (or any parent containing
agent directories) to build every agent that has an agent.codefly.yaml.

Examples:
  cd agents/services/go-generic && codefly agent build
  codefly agent build --dir ./agents/services/go-generic
  cd agents/services && codefly agent build --all`,
	Run: func(cmd *cobra.Command, args []string) {
		all, _ := cmd.Flags().GetBool("all")
		dir, _ := cmd.Flags().GetString("dir")

		skipAudit, _ := cmd.Flags().GetBool("skip-audit")
		failOnVuln, _ := cmd.Flags().GetBool("fail-on-vuln")
		opts := buildOptions{skipAudit: skipAudit, failOnVuln: failOnVuln}

		if all {
			if dir == "" {
				var err error
				dir, err = os.Getwd()
				if err != nil {
					cli.Error("Cannot get working directory: %v", err)
					cli.Exit()
				}
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				cli.Error("Cannot resolve directory: %v", err)
				cli.Exit()
			}
			if err := buildAllAgents(absDir, opts); err != nil {
				cli.Error("Build --all failed: %v", err)
				cli.ExitError()
			}
			return
		}

		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				cli.Error("Cannot get working directory: %v", err)
				cli.Exit()
			}
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			cli.Error("Cannot resolve directory: %v", err)
			cli.Exit()
		}

		if err := buildAgent(absDir, opts); err != nil {
			cli.Error("Build failed: %v", err)
			cli.ExitError()
		}
	},
}

type buildOptions struct {
	skipAudit  bool
	failOnVuln bool
}

// BuildOptions is the exported form of buildOptions for callers in other
// packages (e.g. `codefly self build --with-agents`).
type BuildOptions struct {
	SkipAudit  bool
	FailOnVuln bool
}

// BuildAllAgents builds every agent under root. It is the exported entry point
// for `codefly self build --with-agents`, so one command rebuilds the CLI and
// all agents together. root is typically <monorepo>/agents/services.
func BuildAllAgents(root string, opts BuildOptions) error {
	return buildAllAgents(root, buildOptions{skipAudit: opts.SkipAudit, failOnVuln: opts.FailOnVuln})
}

func init() {
	BuildCmd.Flags().String("dir", "", "Agent source directory (default: current directory)")
	BuildCmd.Flags().Bool("all", false, "Build all agents found in the current directory tree")
	BuildCmd.Flags().Bool("skip-audit", false, "Skip the post-build govulncheck audit")
	BuildCmd.Flags().Bool("fail-on-vuln", false, "Fail the build if any HIGH/CRITICAL vulnerability is found")
}

// quarantinedAgent records an agent skipped by a bulk build because its
// manifest sets quarantine: true.
type quarantinedAgent struct {
	name   string
	reason string
}

// buildAllAgents discovers all directories containing agent.codefly.yaml
// under root and builds each one — SKIPPING agents whose manifest marks them
// quarantine: true (not yet migrated to the new style). Quarantined agents are
// reported so a bulk rebuild is honest about what it did and didn't touch.
func buildAllAgents(root string, opts buildOptions) error {
	var agents []string
	var quarantined []quarantinedAgent
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		yamlPath := filepath.Join(dir, "agent.codefly.yaml")
		data, rerr := os.ReadFile(yamlPath)
		if rerr != nil {
			continue // not an agent directory
		}
		var ag agentYAML
		if uerr := yaml.Unmarshal(data, &ag); uerr != nil {
			// A malformed manifest must NOT silently fall through to the
			// buildable list — that could build an agent meant to be
			// quarantined (whose quarantine flag we just failed to read). Skip
			// it loudly so a broken manifest is visible, not built.
			cli.Info("  ⚠ skipping %s — unreadable agent.codefly.yaml: %v", e.Name(), uerr)
			continue
		}
		if ag.Quarantine {
			quarantined = append(quarantined, quarantinedAgent{name: e.Name(), reason: ag.QuarantineReason})
			continue
		}
		agents = append(agents, dir)
	}

	if len(quarantined) > 0 {
		cli.Header(2, "Skipping %d quarantined agent(s) (not migrated to the new style):", len(quarantined))
		for _, q := range quarantined {
			if q.reason != "" {
				cli.Info("  - %s — %s", q.name, q.reason)
			} else {
				cli.Info("  - %s", q.name)
			}
		}
		cli.Info("  (build one explicitly with `codefly agent build --dir <path>` to work on it)")
	}

	if len(agents) == 0 {
		if len(quarantined) > 0 {
			return fmt.Errorf("no buildable agents under %s — all %d are quarantined", root, len(quarantined))
		}
		return fmt.Errorf("no agent directories found under %s", root)
	}

	cli.Header(1, "Building %d agents", len(agents))
	var failed []string
	for i, agentDir := range agents {
		cli.Header(2, "[%d/%d] %s", i+1, len(agents), filepath.Base(agentDir))
		if err := buildAgent(agentDir, opts); err != nil {
			cli.Error("  Failed: %v", err)
			failed = append(failed, filepath.Base(agentDir))
			continue
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d agent(s) failed to build: %s", len(failed), strings.Join(failed, ", "))
	}

	if len(quarantined) > 0 {
		cli.Header(1, "%d agents built successfully (%d quarantined, skipped)", len(agents), len(quarantined))
	} else {
		cli.Header(1, "All %d agents built successfully", len(agents))
	}
	return nil
}

// monorepoModules lists modules that may need local replace directives
// when building inside the codefly.dev monorepo.
var monorepoModules = []struct {
	Module string
	SubDir string
}{
	{"github.com/codefly-dev/core/wool/otel", "wool/otel"},
	{"github.com/codefly-dev/core/wool", "wool"},
	{"github.com/codefly-dev/core", "core"},
}

// findMonorepoRoot walks up from dir looking for the codefly.dev monorepo
// root (identified by having both wool/ and core/ directories).
func findMonorepoRoot(dir string) string {
	cur := dir
	for {
		woolDir := filepath.Join(cur, "wool")
		coreDir := filepath.Join(cur, "core")
		if isDir(woolDir) && isDir(coreDir) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func goModRequires(dir, module string) bool {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), module) {
			return true
		}
	}
	return false
}

func addReplace(dir, module, localPath string) error {
	cmd := exec.Command("go", "mod", "edit",
		"-replace", fmt.Sprintf("%s=%s", module, localPath))
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dropReplace(dir, module string) error {
	cmd := exec.Command("go", "mod", "edit", "-dropreplace", module)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildAgent(dir string, opts buildOptions) error {
	yamlPath := filepath.Join(dir, "agent.codefly.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("read agent.codefly.yaml in %s: %w (is this an agent directory?)", dir, err)
	}

	var ag agentYAML
	if err := yaml.Unmarshal(data, &ag); err != nil {
		return fmt.Errorf("parse agent.codefly.yaml: %w", err)
	}
	if ag.Name == "" || ag.Version == "" || ag.Publisher == "" {
		return fmt.Errorf("agent.codefly.yaml must have publisher, name, and version")
	}
	// A quarantined agent is excluded from bulk builds; an explicit single
	// build still proceeds (that's how you fix it) but says so loudly.
	if ag.Quarantine {
		if ag.QuarantineReason != "" {
			cli.Info("⚠ %s is QUARANTINED (%s) — building anyway because you asked for it explicitly", ag.Name, ag.QuarantineReason)
		} else {
			cli.Info("⚠ %s is QUARANTINED — building anyway because you asked for it explicitly", ag.Name)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	subdir := "services"
	if ag.Kind == "codefly:application" {
		subdir = "applications"
	} else if ag.Kind == "codefly:module" {
		subdir = "modules"
	}

	nativeDir := filepath.Join(home, ".codefly", "agents", subdir, ag.Publisher)
	binaryName := fmt.Sprintf("%s__%s", ag.Name, ag.Version)
	nativePath := filepath.Join(nativeDir, binaryName)

	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", nativeDir, err)
	}

	// Auto-detect monorepo and add local replace directives.
	// When core is required (directly or transitively), wool and wool/otel
	// must also be replaced because they are not published to a public registry.
	monoRoot := findMonorepoRoot(dir)
	var addedReplaces []string
	if monoRoot != "" {
		cli.Info("Monorepo detected at %s", monoRoot)
		requiresCore := goModRequires(dir, "github.com/codefly-dev/core")
		for _, m := range monorepoModules {
			if !goModRequires(dir, m.Module) && !requiresCore {
				continue
			}
			localPath := filepath.Join(monoRoot, m.SubDir)
			if !isDir(localPath) {
				continue
			}
			cli.Info("  Adding replace: %s => %s", m.Module, localPath)
			if err := addReplace(dir, m.Module, localPath); err != nil {
				return fmt.Errorf("add replace for %s: %w", m.Module, err)
			}
			addedReplaces = append(addedReplaces, m.Module)
		}
	}

	// Clean up replace directives when done (even on error).
	defer func() {
		for _, mod := range addedReplaces {
			_ = dropReplace(dir, mod)
		}
	}()

	cli.Info("Tidying modules...")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Stdout = os.Stdout
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}

	cli.Header(1, "Building %s:%s (%s/%s)", ag.Name, ag.Version, runtime.GOOS, runtime.GOARCH)
	nativeStarted := time.Now()
	build := exec.Command("go", "build", "-o", nativePath, ".")
	build.Dir = dir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build (native): %w", err)
	}
	cli.Info("Binary build done (%s/%s) elapsed=%s", runtime.GOOS, runtime.GOARCH, time.Since(nativeStarted).Round(100*time.Millisecond))
	cli.Info("Installed: %s", nativePath)

	containerDir := filepath.Join(home, ".codefly", "containers", "agents", subdir, ag.Publisher)
	containerPath := filepath.Join(containerDir, binaryName)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", containerDir, err)
	}

	cli.Info("Building Linux/amd64 static binary...")
	containerStarted := time.Now()
	ldflags := `-extldflags "-static"`
	crossBuild := exec.Command("go", "build", "-ldflags", ldflags, "-o", containerPath, ".")
	crossBuild.Dir = dir
	crossBuild.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
	)
	crossBuild.Stdout = os.Stdout
	crossBuild.Stderr = os.Stderr
	if err := crossBuild.Run(); err != nil {
		cli.Info("Warning: Linux cross-build failed (non-fatal): %v", err)
	} else {
		cli.Info("Binary build done (linux/amd64) elapsed=%s", time.Since(containerStarted).Round(100*time.Millisecond))
		cli.Info("Installed (container): %s", containerPath)
	}

	cli.Header(1, "Agent %s:%s built successfully", ag.Name, ag.Version)

	if !opts.skipAudit {
		if err := runAudit(dir, ag, opts.failOnVuln); err != nil {
			return err
		}
	}
	return nil
}

// runAudit runs a govulncheck-based security scan on the agent's Go module.
// Findings are printed to the build log. With --fail-on-vuln, the build
// returns an error if any HIGH/CRITICAL vulnerability is found; otherwise
// the audit is informational only (exit status unchanged).
//
// Findings matching IDs in a workspace-root .govulncheck.yaml are filtered
// out and reported separately as "suppressed (reviewed)".
func runAudit(dir string, ag agentYAML, failOnVuln bool) error {
	cli.Header(1, "Auditing %s:%s for vulnerabilities", ag.Name, ag.Version)
	ctx := context.Background()
	res, err := audit.Golang(ctx, dir, true)
	if err != nil {
		cli.Info("Audit error (non-fatal): %v", err)
		return nil
	}
	if res.Tool == "missing" {
		cli.Info("govulncheck not installed — skipping. Install: go install golang.org/x/vuln/cmd/govulncheck@latest")
		return nil
	}
	cli.Info("Tool: %s", res.Tool)

	suppressions := loadSuppressions(dir)

	// Partition findings: suppressed (workspace policy), actionable
	// (upstream has a patch), unpatched (upstream has no fix yet).
	// --fail-on-vuln only blocks on actionable.
	var actionable, unpatched, suppressed []*builderv0.AuditFinding
	for _, f := range res.Findings {
		if _, ok := suppressions[f.Id]; ok {
			suppressed = append(suppressed, f)
			continue
		}
		if f.FixedVersion == "" {
			unpatched = append(unpatched, f)
		} else {
			actionable = append(actionable, f)
		}
	}

	if len(actionable)+len(unpatched) == 0 {
		if len(suppressed) > 0 {
			cli.Info("No actionable vulnerabilities. %d suppressed by .govulncheck.yaml (reviewed).", len(suppressed))
		} else {
			cli.Info("No vulnerabilities found.")
		}
	} else {
		if len(actionable) > 0 {
			cli.Info("Vulnerabilities with available fixes: %d", len(actionable))
			for _, f := range actionable {
				cli.Info("  [%s] %s %s@%s → fixed in %s (%s)",
					severityLabel(f.Severity), f.Id, f.Package, f.CurrentVersion,
					f.FixedVersion, truncate(f.Summary, 80))
			}
		}
		if len(unpatched) > 0 {
			cli.Info("Unpatched upstream (tracked, no action available): %d", len(unpatched))
			for _, f := range unpatched {
				cli.Info("  [%s] %s %s@%s — no upstream fix yet (%s)",
					severityLabel(f.Severity), f.Id, f.Package, f.CurrentVersion,
					truncate(f.Summary, 80))
			}
		}
		if len(suppressed) > 0 {
			cli.Info("Suppressed (reviewed via .govulncheck.yaml): %d", len(suppressed))
		}
	}
	if len(res.Outdated) > 0 {
		cli.Info("Outdated packages: %d (run `codefly upgrade agent --dry-run` to preview)",
			len(res.Outdated))
	}

	if failOnVuln {
		// Only block on actionable findings — unpatched upstream vulns can't
		// be resolved by a rebuild, so failing the build on them just blocks
		// development without improving security.
		var blockers int
		for _, f := range actionable {
			if f.Severity == builderv0.AuditFinding_HIGH ||
				f.Severity == builderv0.AuditFinding_CRITICAL {
				blockers++
			}
		}
		if blockers > 0 {
			return fmt.Errorf("audit failed: %d high/critical vulnerability(ies) with available upstream fixes in %s:%s (use --skip-audit to bypass)",
				blockers, ag.Name, ag.Version)
		}
	}
	return nil
}

// loadSuppressions walks up from dir looking for a .govulncheck.yaml and
// returns the set of suppressed vuln IDs. Missing file or parse errors are
// silently ignored — suppressions are optional, not required.
func loadSuppressions(dir string) map[string]struct{} {
	cur := dir
	for {
		p := filepath.Join(cur, ".govulncheck.yaml")
		data, err := os.ReadFile(p)
		if err == nil {
			var doc struct {
				Suppressions []struct {
					ID string `yaml:"id"`
				} `yaml:"suppressions"`
			}
			if yaml.Unmarshal(data, &doc) == nil {
				m := make(map[string]struct{}, len(doc.Suppressions))
				for _, s := range doc.Suppressions {
					if s.ID != "" {
						m[s.ID] = struct{}{}
					}
				}
				return m
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

func severityLabel(s builderv0.AuditFinding_Severity) string {
	switch s {
	case builderv0.AuditFinding_CRITICAL:
		return "CRITICAL"
	case builderv0.AuditFinding_HIGH:
		return "HIGH"
	case builderv0.AuditFinding_MEDIUM:
		return "MEDIUM"
	case builderv0.AuditFinding_LOW:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
