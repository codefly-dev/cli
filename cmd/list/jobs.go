package list

import (
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
	Run: func(cmd *cobra.Command, args []string) {
		listJobs()
	},
}

func listJobs() {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)

	if listJobsModule != "" {
		// List jobs in specific module
		mod, err := workspace.LoadModuleFromName(ctx, listJobsModule)
		if err != nil {
			cli.ExitOnError(err, "module not found")
		}

		jobs, err := mod.LoadJobs(ctx)
		if err != nil {
			cli.ExitOnError(err, "failed to load jobs")
		}

		if len(jobs) == 0 {
			cli.Info("No jobs found in module <%s>", listJobsModule)
			return
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
		return
	}

	// List all jobs
	jobs, err := workspace.LoadAllJobs(ctx)
	if err != nil {
		cli.ExitOnError(err, "failed to load jobs")
	}

	if len(jobs) == 0 {
		cli.Info("No jobs found in workspace <%s>", workspace.Name)
		return
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
}

func init() {
	JobsCmd.Flags().StringVar(&listJobsModule, "module", "", "Module to list jobs from")
}
