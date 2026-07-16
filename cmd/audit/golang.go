package audit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services/audit"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// defaultStaleAfterDays mirrors the bash script's STALE_AFTER_DAYS
// default: a suppression whose `reviewed:` date is older than this is
// treated as expired so a human re-checks whether the upstream shipped a
// patch or the reachability analysis still holds.
const defaultStaleAfterDays = 45

var (
	goAuditDir       string
	goAuditStaleDays int
	goAuditFail      bool
)

// GoCmd implements `codefly audit go` — the Go-module vulnerability gate
// that replaces scripts/govulncheck.sh. It runs govulncheck against the
// module at --dir, applies the workspace .govulncheck.yaml suppressions,
// fails on STALE suppressions, and exits non-zero only when an
// UNSUPPRESSED finding has an upstream fix available (actionable).
// Unpatched-upstream findings are reported but don't block — no rebuild
// can clear them.
var GoCmd = &cobra.Command{
	Use:   "go",
	Short: "Audit a Go module with govulncheck (suppressions + staleness gate)",
	Long: `Audit the Go module at --dir (default: current directory) with govulncheck.

Findings are partitioned against the nearest .govulncheck.yaml (walking up
from --dir):
  - suppressed  — id listed in .govulncheck.yaml (reviewed, tracked)
  - actionable  — an upstream fix is available (BLOCKS with --fail-on-vuln)
  - unpatched   — no upstream fix yet (reported, never blocks)

Suppression entries whose 'reviewed:' date is older than --stale-days fail
the command so the rationale gets re-verified.

This replaces scripts/govulncheck.sh; CI calls it after building the CLI.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// The go audit needs no workspace/agent/tracing machinery, so it
		// stays usable from a plain repo checkout in
		// CI (no workspace.codefly.yaml required).
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		dir := goAuditDir
		if dir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot read working directory: %w", err)
			}
			dir = wd
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("cannot resolve --dir: %w", err)
		}

		blocked, err := RunGoAudit(ctx, abs, goAuditStaleDays, goAuditFail)
		if err != nil {
			return fmt.Errorf("audit failed: %w", err)
		}
		if blocked {
			return fmt.Errorf("audit found actionable vulnerabilities or stale suppressions")
		}
		cli.Done()
		return nil
	},
}

// truncate shortens s to at most n bytes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// Suppression is one entry from .govulncheck.yaml.
type Suppression struct {
	ID       string `yaml:"id"`
	Module   string `yaml:"module"`
	Reason   string `yaml:"reason"`
	Reviewed string `yaml:"reviewed"`
}

// LoadSuppressions walks up from dir looking for .govulncheck.yaml and
// returns its entries plus the path it was found at (empty if none). A
// missing file is not an error — suppressions are optional.
func LoadSuppressions(dir string) ([]Suppression, string, error) {
	cur := dir
	for {
		p := filepath.Join(cur, ".govulncheck.yaml")
		data, err := os.ReadFile(p)
		if err == nil {
			var doc struct {
				Suppressions []Suppression `yaml:"suppressions"`
			}
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return nil, p, fmt.Errorf("parse %s: %w", p, err)
			}
			return doc.Suppressions, p, nil
		}
		if !os.IsNotExist(err) {
			return nil, p, fmt.Errorf("read %s: %w", p, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil, "", nil
		}
		cur = parent
	}
}

// staleSuppressions returns "id(age days)" descriptions for entries whose
// reviewed date is older than staleDays. Entries with an unparseable or
// missing reviewed date are skipped (warned, not failed) — same lenient
// behavior as the bash script.
func staleSuppressions(supps []Suppression, staleDays int) []string {
	if staleDays <= 0 {
		return nil
	}
	var stale []string
	now := time.Now()
	for _, s := range supps {
		if s.ID == "" || s.Reviewed == "" {
			continue
		}
		reviewed, err := time.Parse("2006-01-02", s.Reviewed)
		if err != nil {
			cli.Warning("%s: cannot parse reviewed date %q — skipping staleness check", s.ID, s.Reviewed)
			continue
		}
		age := int(now.Sub(reviewed).Hours() / 24)
		if age > staleDays {
			stale = append(stale, fmt.Sprintf("%s(%d days)", s.ID, age))
		}
	}
	return stale
}

// RunGoAudit runs the govulncheck gate for the module at dir and reports
// findings. Returns (blocked, error): blocked is true when failOnVuln is
// set and there is an actionable (upstream-fixed) finding or a stale
// suppression. error is reserved for genuine run failures (govulncheck
// crash, unreadable suppressions). Exposed so `codefly update deps` can
// reuse the same gate after bumping dependencies.
func RunGoAudit(ctx context.Context, dir string, staleDays int, failOnVuln bool) (bool, error) {
	cli.Header(1, "Auditing Go module at %s", dir)

	res, err := audit.Golang(ctx, dir, false)
	if err != nil {
		return false, err
	}
	cli.Info("Tool: %s", res.Tool)

	supps, suppPath, err := LoadSuppressions(dir)
	if err != nil {
		return false, err
	}
	suppressed := make(map[string]struct{}, len(supps))
	for _, s := range supps {
		if s.ID != "" {
			suppressed[s.ID] = struct{}{}
		}
	}
	if suppPath != "" {
		cli.Info("Loaded %d suppression(s) from %s", len(suppressed), suppPath)
	}

	// A stale suppression blocks (with --fail-on-vuln) regardless of whether
	// it currently matches a finding: the point is to force periodic review.
	stale := staleSuppressions(supps, staleDays)
	if len(stale) > 0 {
		cli.Error("STALE suppressions (>%d days since review): %v", staleDays, stale)
		cli.ErrorDetail("Re-verify the rationale, bump 'reviewed:' in .govulncheck.yaml, and commit.")
	}

	// govulncheck emits one finding per reachable call path, so the same
	// OSV id recurs many times. Dedupe by id (keeping the first trace) so
	// counts and output reflect distinct vulnerabilities — matching the
	// per-"Vulnerability #N" granularity of the old bash script.
	var actionable, unpatched, suppressedHits []*builderv0.AuditFinding
	seen := make(map[string]struct{}, len(res.Findings))
	for _, f := range res.Findings {
		if _, dup := seen[f.Id]; dup {
			continue
		}
		seen[f.Id] = struct{}{}
		if _, ok := suppressed[f.Id]; ok {
			suppressedHits = append(suppressedHits, f)
			continue
		}
		if f.FixedVersion == "" {
			unpatched = append(unpatched, f)
		} else {
			actionable = append(actionable, f)
		}
	}

	if len(actionable) > 0 {
		cli.Info("Actionable vulnerabilities (upstream fix available): %d", len(actionable))
		for _, f := range actionable {
			cli.Info("  %s %s@%s → fixed in %s (%s)",
				f.Id, f.Package, f.CurrentVersion, f.FixedVersion, truncate(f.Summary, 80))
		}
	}
	if len(unpatched) > 0 {
		cli.Info("Unpatched upstream (tracked, no fix yet): %d", len(unpatched))
		for _, f := range unpatched {
			cli.Info("  %s %s@%s — no upstream fix yet (%s)",
				f.Id, f.Package, f.CurrentVersion, truncate(f.Summary, 80))
		}
	}
	if len(suppressedHits) > 0 {
		cli.Info("Suppressed (reviewed via .govulncheck.yaml): %d", len(suppressedHits))
	}
	if len(actionable) == 0 && len(unpatched) == 0 && len(suppressedHits) == 0 {
		cli.Info("No vulnerabilities found.")
	}

	if failOnVuln && (len(actionable) > 0 || len(stale) > 0) {
		return true, nil
	}
	return false, nil
}

func init() {
	GoCmd.Flags().StringVar(&goAuditDir, "dir", "", "Go module directory to audit (default: current directory)")
	GoCmd.Flags().IntVar(&goAuditStaleDays, "stale-days", defaultStaleAfterDays, "Fail when a suppression's reviewed date is older than this many days (0 disables)")
	GoCmd.Flags().BoolVar(&goAuditFail, "fail-on-vuln", true, "Exit non-zero on an actionable finding or stale suppression")
}
