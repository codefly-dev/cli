package list

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var listJobsModule string

// JobsCmd represents the list jobs command
var JobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "List all jobs in the workspace",
	Long: `List all jobs in the workspace or a specific module.

Examples:
  # List all jobs
  codefly list jobs

  # List jobs in a specific module
	codefly list jobs --module=backend
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return listJobs()
	},
}

func listJobs() error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	if listJobsModule != "" {
		// List jobs in specific module
		mod, err := workspace.LoadModuleFromName(ctx, listJobsModule)
		if err != nil {
			return fmt.Errorf("module not found: %w", err)
		}

		jobs, err := mod.LoadJobs(ctx)
		if err != nil {
			return fmt.Errorf("failed to load jobs: %w", err)
		}

		if len(jobs) == 0 {
			cli.Info("No jobs found in module <%s>", listJobsModule)
			return nil
		}

		cli.Header(2, "Jobs in module <%s>:", listJobsModule)
		for _, job := range jobs {
			execType := "one-shot"
			if job.Execution != nil {
				execType = string(job.Execution.Type)
			}
			cli.Info("  - %s (v%s) [%s]", job.Name, job.Version, execType)
			if job.Description != "" {
				cli.Info("    %s", job.Description)
			}
		}
		return nil
	}

	// List all jobs
	jobs, err := workspace.LoadAllJobs(ctx)
	if err != nil {
		return fmt.Errorf("failed to load jobs: %w", err)
	}

	if len(jobs) == 0 {
		cli.Info("No jobs found in workspace <%s>", workspace.Name)
		return nil
	}

	cli.Header(2, "Jobs in workspace <%s>:", workspace.Name)
	for _, job := range jobs {
		execType := "one-shot"
		if job.Execution != nil {
			execType = string(job.Execution.Type)
		}
		cli.Info("  - %s/%s (v%s) [%s]", job.Module(), job.Name, job.Version, execType)
		if job.Description != "" {
			cli.Info("    %s", job.Description)
		}
	}
	return nil
}

func init() {
	JobsCmd.Flags().StringVar(&listJobsModule, "module", "", "Module to list jobs from")
}
