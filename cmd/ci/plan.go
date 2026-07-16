package ci

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/spf13/cobra"
)

var (
	planBase         string
	planHead         string
	planChangedFiles []string
	planAll          bool
	planFormat       string
)

// PlanCmd exposes Codefly's provider-neutral changed/affected service plan.
// It performs no validation and starts no agents, so CI systems and developers
// can inspect exactly what a subsequent Codefly-native run would select.
var PlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show directly changed and transitively affected services",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}

		changed := append([]string(nil), planChangedFiles...)
		if len(changed) == 0 {
			changed = append(changed, changedFilesFromEnvironment()...)
		}
		base := firstNonEmpty(planBase, os.Getenv("CODEFLY_CI_BASE"))
		head := firstNonEmpty(planHead, os.Getenv("CODEFLY_CI_HEAD"))

		plan, err := BuildPlan(ctx, workspace, PlanOptions{
			Base:         base,
			Head:         head,
			ChangedFiles: changed,
			All:          planAll,
		})
		if err != nil {
			return fmt.Errorf("cannot build CI plan: %w", err)
		}

		switch strings.ToLower(strings.TrimSpace(planFormat)) {
		case "", "text":
			printPlan(plan)
		case "json":
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(plan); err != nil {
				return fmt.Errorf("encode CI plan: %w", err)
			}
		default:
			return fmt.Errorf("unsupported format %q (use text or json)", planFormat)
		}
		return nil
	},
}

func init() {
	PlanCmd.Flags().StringVar(&planBase, "base", "", "Base Git revision for change discovery")
	PlanCmd.Flags().StringVar(&planHead, "head", "", "Head Git revision (defaults to HEAD when --base is set)")
	PlanCmd.Flags().StringSliceVar(&planChangedFiles, "changed-file", nil, "Changed path supplied by the CI provider (repeatable; bypasses Git discovery)")
	PlanCmd.Flags().BoolVar(&planAll, "all", false, "Select every service explicitly")
	PlanCmd.Flags().StringVar(&planFormat, "format", "text", "Output format: text or json")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func changedFilesFromEnvironment() []string {
	raw := os.Getenv("CODEFLY_CI_CHANGED_FILES")
	if raw == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == '\x00' })
}

func printPlan(plan *Plan) {
	fmt.Printf("Codefly CI plan for %s\n", plan.Workspace)
	if plan.Base != "" || plan.Head != "" {
		fmt.Printf("Revisions: %s..%s\n", plan.Base, plan.Head)
	}
	if plan.SelectionReason != "" {
		fmt.Printf("Selection: %s\n", plan.SelectionReason)
	}
	if len(plan.ChangedFiles) == 0 {
		fmt.Println("Changed files: none supplied or discovered")
	} else {
		fmt.Printf("Changed files (%d):\n", len(plan.ChangedFiles))
		for _, path := range plan.ChangedFiles {
			fmt.Printf("  - %s\n", path)
		}
	}
	if len(plan.Services) == 0 {
		fmt.Println("Services: none affected")
		return
	}
	fmt.Printf("Services (%d, dependency order):\n", len(plan.Services))
	for _, service := range plan.Services {
		reason := strings.Join(service.Reasons, "; ")
		fmt.Printf("  - %-10s %s", service.Classification, service.Service)
		if reason != "" {
			fmt.Printf(" — %s", reason)
		}
		fmt.Println()
	}
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
