package sync

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	libSyncService string
	libSyncModule  string
	libSyncCleanup bool
)

// LibraryDependenciesCmd represents the sync library-dependencies command
var LibraryDependenciesCmd = &cobra.Command{
	Use:   "library-dependencies",
	Short: "Setup local development for library dependencies",
	Long: `Configure local development for library dependencies of a service.

This will:
- For Go: Add replace directives to go.mod pointing to local library paths
- For Python: Install libraries in editable mode (pip install -e)
- For TypeScript/Node: Set up npm link

Examples:
  # Setup local dev for all library dependencies of a service
  codefly sync library-dependencies --service=api --module=backend

  # Cleanup local development setup (for production builds)
  codefly sync library-dependencies --service=api --module=backend --cleanup
`,
	Run: func(cmd *cobra.Command, args []string) {
		syncLibraryDependencies()
	},
}

func syncLibraryDependencies() {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)

	// Get service
	if libSyncModule == "" || libSyncService == "" {
		// Try to get from active context
		active := common.Service(ctx)
		if active != nil {
			libSyncModule = common.Module(ctx).Name
			libSyncService = active.Name
		} else {
			cli.Error("Please specify --service and --module")
			os.Exit(1)
		}
	}

	mod, err := workspace.LoadModuleFromName(ctx, libSyncModule)
	if err != nil {
		cli.ExitOnError(err, "module not found")
	}

	svc, err := mod.LoadServiceFromName(ctx, libSyncService)
	if err != nil {
		cli.ExitOnError(err, "service not found")
	}

	if len(svc.LibraryDependencies) == 0 {
		cli.Info("Service <%s/%s> has no library dependencies", libSyncModule, libSyncService)
		return
	}

	resolver := resources.NewLibraryResolver(workspace)

	if libSyncCleanup {
		cli.Info("Cleaning up local development setup for <%s/%s>...", libSyncModule, libSyncService)
		if err := resolver.CleanupLocalDevelopment(ctx, svc); err != nil {
			cli.ExitOnError(err, "failed to cleanup local development")
		}
		cli.Header(2, "Local development cleanup complete")
		return
	}

	cli.Info("Setting up local development for <%s/%s>...", libSyncModule, libSyncService)
	cli.Info("Library dependencies:")
	for _, dep := range svc.LibraryDependencies {
		cli.Info("  - %s (%s) [%v]", dep.Name, dep.Version, dep.Languages)
	}

	if err := resolver.SetupLocalDevelopment(ctx, svc); err != nil {
		cli.ExitOnError(err, "failed to setup local development")
	}

	cli.Header(2, "Local development setup complete for %s/%s", libSyncModule, libSyncService)
}

func init() {
	LibraryDependenciesCmd.Flags().StringVar(&libSyncService, "service", "", "Service name")
	LibraryDependenciesCmd.Flags().StringVar(&libSyncModule, "module", "", "Module name")
	LibraryDependenciesCmd.Flags().BoolVar(&libSyncCleanup, "cleanup", false, "Remove local development setup (for production)")
}
