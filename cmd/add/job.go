package add

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	jobModule       string
	jobAgent        string
	jobExecutionType string
	jobSchedule     string
	jobTimeout      string
)

// JobCmd represents the add job command
var JobCmd = &cobra.Command{
	Use:   "job [name]",
	Short: "Add a job to a module",
	Long: `Add a job (scheduled or one-shot task) to a module.

Jobs are ephemeral execution units for tasks like:
- Database migrations
- Data processing / ETL
- Scheduled tasks (cleanup, reports)
- Deployment tasks

Examples:
  # Create a one-shot job
  codefly add job db-migration --module=backend

  # Create a scheduled job with cron expression
  codefly add job cleanup --module=backend --type=scheduled --schedule="0 0 * * *"

  # Create a job with specific timeout
  codefly add job data-import --module=backend --timeout=1h
`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		addJob(name)
	},
}

func addJob(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)

	// Get module
	if jobModule == "" {
		// Try to get from active context
		mod := common.Module(ctx)
		if mod != nil {
			jobModule = mod.Name
		} else {
			cli.Error("Please specify --module")
			os.Exit(1)
		}
	}

	mod, err := workspace.LoadModuleFromName(ctx, jobModule)
	if err != nil {
		cli.ExitOnError(err, "module not found")
	}

	// Check if job already exists
	_, err = mod.LoadJobFromName(ctx, name)
	if err == nil {
		cli.Error("Job <%s> already exists in module <%s>", name, jobModule)
		os.Exit(1)
	}

	// Determine execution type
	execType := resources.JobExecutionOneShot
	if jobExecutionType != "" {
		switch jobExecutionType {
		case "one-shot":
			execType = resources.JobExecutionOneShot
		case "scheduled":
			execType = resources.JobExecutionScheduled
		case "triggered":
			execType = resources.JobExecutionTriggered
		default:
			cli.Error("Invalid execution type: %s (use one-shot, scheduled, or triggered)", jobExecutionType)
			os.Exit(1)
		}
	}

	// Validate scheduled jobs have a schedule
	if execType == resources.JobExecutionScheduled && jobSchedule == "" {
		cli.Error("Scheduled jobs require --schedule with a cron expression")
		os.Exit(1)
	}

	// Default timeout
	timeout := jobTimeout
	if timeout == "" {
		timeout = "30m"
	}

	confirm := models.Confirm(ctx,
		fmt.Sprintf("Add job <%s> to module <%s>?", name, jobModule),
		true)
	if !confirm {
		cli.Header(2, "Cancelled.")
		os.Exit(0)
	}

	// Create the job
	job, err := mod.NewJob(ctx, name)
	if err != nil {
		cli.ExitOnError(err, "cannot create job")
	}

	// Configure execution
	job.Execution = &resources.JobExecution{
		Type:     execType,
		Schedule: jobSchedule,
		Timeout:  timeout,
		Retries:  0,
	}

	// Set agent if specified
	if jobAgent != "" {
		job.Agent = &resources.Agent{
			Kind:    resources.JobAgent,
			Name:    jobAgent,
			Version: "0.0.1",
		}
	}

	// Save job
	if err := job.Save(ctx); err != nil {
		cli.ExitOnError(err, "failed to save job")
	}

	// Add job reference to module
	if err := mod.AddJobReference(ctx, &resources.JobReference{Name: name}); err != nil {
		cli.ExitOnError(err, "failed to add job reference to module")
	}

	// Save module
	if err := mod.Save(ctx); err != nil {
		cli.ExitOnError(err, "failed to save module")
	}

	cli.Header(2, "Job <%s> created in module <%s>", name, jobModule)
	cli.Info("Path: %s", job.Dir())
	cli.Info("")
	cli.Info("Next steps:")
	cli.Info("  1. Configure the job agent in %s/job.codefly.yaml", job.Dir())
	cli.Info("  2. Add your job code")
	cli.Info("  3. Run with: codefly run job %s --module=%s", name, jobModule)
}

func init() {
	JobCmd.Flags().StringVar(&jobModule, "module", "", "Module name")
	JobCmd.Flags().StringVar(&jobAgent, "agent", "", "Job agent (e.g., go-job)")
	JobCmd.Flags().StringVar(&jobExecutionType, "type", "one-shot", "Execution type (one-shot, scheduled, triggered)")
	JobCmd.Flags().StringVar(&jobSchedule, "schedule", "", "Cron schedule for scheduled jobs")
	JobCmd.Flags().StringVar(&jobTimeout, "timeout", "30m", "Job timeout (e.g., 5m, 1h)")
}
