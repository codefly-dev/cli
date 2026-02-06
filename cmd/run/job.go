package run

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var (
	runJobModule     string
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
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		runJob(name)
	},
}

func runJob(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)

	// Get module
	if runJobModule == "" {
		// Try to get from active context
		mod := common.Module(ctx)
		if mod != nil {
			runJobModule = mod.Name
		} else {
			cli.Error("Please specify --module")
			os.Exit(1)
		}
	}

	mod, err := workspace.LoadModuleFromName(ctx, runJobModule)
	if err != nil {
		cli.ExitOnError(err, "module not found")
	}

	job, err := mod.LoadJobFromName(ctx, name)
	if err != nil {
		cli.ExitOnError(err, "job not found")
	}

	cli.Header(2, "Running job <%s/%s>", runJobModule, name)
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
}

func init() {
	JobCmd.Flags().StringVar(&runJobModule, "module", "", "Module name")
	JobCmd.Flags().BoolVar(&runJobWithServices, "with-services", false, "Start service dependencies before running job")
}
