package sync

import (
	"fmt"

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
	Short: "Prepare local development links for a service's internal libraries",
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
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return syncLibraryDependencies()
	},
}

func syncLibraryDependencies() error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	// Get service
	moduleName, serviceName := libSyncModule, libSyncService
	if moduleName == "" || serviceName == "" {
		// Try to get from active context
		active, loadErr := common.LoadActiveContext(ctx)
		if loadErr != nil || active.Module == nil || active.Service == nil {
			return fmt.Errorf("please specify --service and --module")
		}
		moduleName, serviceName = active.Module.Name, active.Service.Name
	}

	mod, err := workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return fmt.Errorf("module not found: %w", err)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("service not found: %w", err)
	}

	if len(svc.LibraryDependencies) == 0 {
		cli.Info("Service <%s/%s> has no library dependencies", moduleName, serviceName)
		return nil
	}

	resolver := resources.NewLibraryResolver(workspace)

	if libSyncCleanup {
		cli.Info("Cleaning up local development setup for <%s/%s>...", moduleName, serviceName)
		if err := resolver.CleanupLocalDevelopment(ctx, svc); err != nil {
			return fmt.Errorf("failed to cleanup local development: %w", err)
		}
		cli.Header(2, "Local development cleanup complete")
		return nil
	}

	cli.Info("Setting up local development for <%s/%s>...", moduleName, serviceName)
	cli.Info("Library dependencies:")
	for _, dep := range svc.LibraryDependencies {
		cli.Info("  - %s (%s) [%v]", dep.Name, dep.Version, dep.Languages)
	}

	if err := resolver.SetupLocalDevelopment(ctx, svc); err != nil {
		return fmt.Errorf("failed to setup local development: %w", err)
	}

	cli.Header(2, "Local development setup complete for %s/%s", moduleName, serviceName)
	return nil
}

func init() {
	LibraryDependenciesCmd.Flags().StringVar(&libSyncService, "service", "", "Service name")
	LibraryDependenciesCmd.Flags().StringVar(&libSyncModule, "module", "", "Module name")
	LibraryDependenciesCmd.Flags().BoolVar(&libSyncCleanup, "cleanup", false, "Remove local development setup (for production)")
}
