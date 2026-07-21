package add

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	jobModule        string
	jobAgent         string
	jobExecutionType string
	jobSchedule      string
	jobTimeout       string
)

// JobCmd represents the add job command
var JobCmd = &cobra.Command{
	Use:   "job [name]",
	Short: "Create a scheduled or one-shot job in a module",
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
	RunE: func(cmd *cobra.Command, args []string) error {
		return addJob(args[0])
	},
}

func addJob(name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	// Get module
	moduleName := jobModule
	if moduleName == "" {
		// Try to get from active context
		activeModule, loadErr := common.LoadModule(ctx)
		if loadErr != nil {
			return fmt.Errorf("please specify --module: %w", loadErr)
		}
		moduleName = activeModule.Name
	}

	mod, err := workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return fmt.Errorf("module not found: %w", err)
	}

	// Check if job already exists
	for _, ref := range mod.JobReferences {
		if resources.ReferenceMatch(ref.Name, name) {
			return fmt.Errorf("job <%s> already exists in module <%s>", name, moduleName)
		}
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
			return fmt.Errorf("invalid execution type %q (use one-shot, scheduled, or triggered)", jobExecutionType)
		}
	}

	// Validate scheduled jobs have a schedule
	if execType == resources.JobExecutionScheduled && jobSchedule == "" {
		return fmt.Errorf("scheduled jobs require --schedule with a cron expression")
	}

	// Default timeout
	timeout := jobTimeout
	if timeout == "" {
		timeout = "30m"
	}

	confirm := models.Confirm(ctx,
		fmt.Sprintf("Add job <%s> to module <%s>?", name, moduleName),
		true)
	if !confirm {
		cli.Header(2, "Cancelled.")
		return nil
	}

	// Create the job
	job, err := mod.NewJob(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot create job: %w", err)
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
		return fmt.Errorf("failed to save job: %w", err)
	}

	// Add job reference to module
	if err := mod.AddJobReference(ctx, &resources.JobReference{Name: name}); err != nil {
		return fmt.Errorf("failed to add job reference to module: %w", err)
	}

	// Save module
	if err := mod.Save(ctx); err != nil {
		return fmt.Errorf("failed to save module: %w", err)
	}

	cli.Header(2, "Job <%s> created in module <%s>", name, moduleName)
	cli.Info("Path: %s", job.Dir())
	cli.Info("")
	cli.Info("Next steps:")
	cli.Info("  1. Configure the job agent in %s/job.codefly.yaml", job.Dir())
	cli.Info("  2. Add your job code")
	cli.Info("  3. Run with: codefly run job %s --module=%s", name, moduleName)
	return nil
}

func init() {
	JobCmd.Flags().StringVar(&jobModule, "module", "", "Module name")
	JobCmd.Flags().StringVar(&jobAgent, "agent", "", "Job agent (e.g., go-job)")
	JobCmd.Flags().StringVar(&jobExecutionType, "type", "one-shot", "Execution type (one-shot, scheduled, triggered)")
	JobCmd.Flags().StringVar(&jobSchedule, "schedule", "", "Cron schedule for scheduled jobs")
	JobCmd.Flags().StringVar(&jobTimeout, "timeout", "30m", "Job timeout (e.g., 5m, 1h)")
}
