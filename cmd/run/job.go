package run

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var (
	runJobModule       string
	runJobWithServices bool
)

// JobCmd represents the run job command
var JobCmd = &cobra.Command{
	Use:   "job [name]",
	Short: "Run a job",
	Long: `Run a job (scheduled or one-shot task).

Jobs execute to completion and then exit. They can depend on services
which will be started before the job runs.

Examples:
  # Run a job
  codefly run job db-migration --module=backend

  # Run a job with its service dependencies started
  codefly run job db-migration --module=backend --with-services
`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runJob(args[0])
	},
}

func runJob(name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	// Get module
	moduleName := runJobModule
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

	job, err := mod.LoadJobFromName(ctx, name)
	if err != nil {
		return fmt.Errorf("job not found: %w", err)
	}

	cli.Header(2, "Running job <%s/%s>", moduleName, name)
	cli.Info("Version: %s", job.Version)
	if job.Description != "" {
		cli.Info("Description: %s", job.Description)
	}
	if job.Execution != nil {
		cli.Info("Execution type: %s", job.Execution.Type)
		cli.Info("Timeout: %s", job.Execution.Timeout)
	}

	// Check service dependencies
	if len(job.ServiceDependencies) > 0 {
		cli.Info("")
		cli.Info("Service dependencies:")
		for _, dep := range job.ServiceDependencies {
			cli.Info("  - %s/%s", dep.Module, dep.Name)
		}
		if !runJobWithServices {
			cli.Info("")
			cli.Info("Note: Use --with-services to start service dependencies")
		}
	}

	// TODO: Full job execution with orchestration
	// This would involve:
	// 1. Starting service dependencies (if --with-services)
	// 2. Loading job agent
	// 3. Running job lifecycle (Load -> Init -> Execute -> Stop)
	// 4. Handling retries on failure
	// 5. Respecting timeout

	cli.Info("")
	cli.Warning("Job execution not yet fully implemented.")
	cli.Info("Job configuration loaded successfully.")
	cli.Info("Agent: %v", job.Agent)
	return nil
}

func init() {
	JobCmd.Flags().StringVar(&runJobModule, "module", "", "Module name")
	JobCmd.Flags().BoolVar(&runJobWithServices, "with-services", false, "Start service dependencies before running job")
}
