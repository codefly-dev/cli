package ci

import (
	"context"
	"os"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// SelectionFlags is the provider-neutral changed/affected input shared by CI
// phases. CI providers only translate their event metadata into these inputs;
// Codefly remains the owner of resource classification and dependency closure.
type SelectionFlags struct {
	base         string
	head         string
	changedFiles []string
	all          bool
}

func (flags *SelectionFlags) Bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&flags.base, "base", "", "Base Git revision for affected-service discovery")
	cmd.Flags().StringVar(&flags.head, "head", "", "Head Git revision (defaults to HEAD when --base is set)")
	cmd.Flags().StringSliceVar(&flags.changedFiles, "changed-file", nil, "Changed path supplied by the CI provider (repeatable; bypasses Git discovery)")
	cmd.Flags().BoolVar(&flags.all, "all", false, "Select every service explicitly")
}

func (flags *SelectionFlags) BuildPlan(ctx context.Context, workspace *resources.Workspace) (*Plan, error) {
	changed := append([]string(nil), flags.changedFiles...)
	if len(changed) == 0 {
		changed = append(changed, changedFilesFromEnvironment()...)
	}
	return BuildPlan(ctx, workspace, PlanOptions{
		Base:         firstNonEmpty(flags.base, os.Getenv("CODEFLY_CI_BASE")),
		Head:         firstNonEmpty(flags.head, os.Getenv("CODEFLY_CI_HEAD")),
		ChangedFiles: changed,
		All:          flags.all,
	})
}
