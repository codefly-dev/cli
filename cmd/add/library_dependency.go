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
	libDepService    string
	libDepModule     string
	libDepVersion    string
	libDepLanguages  []string
	libDepSetupLocal bool
)

// LibraryDependencyCmd represents the add library-dependency command
var LibraryDependencyCmd = &cobra.Command{
	Use:   "library-dependency [library-name]",
	Short: "Add a library dependency to a service",
	Long: `Add an internal library as a dependency to a service.

This will:
1. Add the library to the service's library-dependencies in service.codefly.yaml
2. Optionally set up local development (go.mod replace, pip -e, npm link)

Examples:
  # Add library dependency with version constraint
  codefly add library-dependency shared-models --service=api --module=backend --version="^1.0.0"

  # Add with automatic local development setup
  codefly add library-dependency shared-models --service=api --module=backend --setup-local

  # Specify which language exports to use
  codefly add library-dependency shared-models --service=api --module=backend --languages=go
`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return addLibraryDependency(args[0])
	},
}

func addLibraryDependency(libraryName string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	// Validate library exists
	lib, err := workspace.LoadLibraryFromName(ctx, libraryName)
	if err != nil {
		return fmt.Errorf("library not found: %w", err)
	}

	// Get service
	moduleName, serviceName := libDepModule, libDepService
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

	// Check if dependency already exists
	for _, dep := range svc.LibraryDependencies {
		if dep.Name == libraryName {
			return fmt.Errorf("service already depends on library <%s>", libraryName)
		}
	}

	// Determine languages
	languages := libDepLanguages
	if len(languages) == 0 {
		// Default to all languages the library supports
		for _, lang := range lib.Languages {
			languages = append(languages, lang.Name)
		}
	}

	// Default version constraint
	version := libDepVersion
	if version == "" {
		version = fmt.Sprintf("^%s", lib.Version)
	}

	confirm := models.Confirm(ctx,
		fmt.Sprintf("Add library <%s> (%s) as dependency to service <%s/%s>?",
			libraryName, version, moduleName, serviceName),
		true)
	if !confirm {
		cli.Header(2, "Cancelled.")
		return nil
	}

	// Add the dependency
	svc.LibraryDependencies = append(svc.LibraryDependencies, &resources.LibraryDependency{
		Name:      libraryName,
		Version:   version,
		Languages: languages,
	})

	// Save service
	if err := svc.Save(ctx); err != nil {
		return fmt.Errorf("failed to save service: %w", err)
	}

	cli.Header(2, "Library dependency added to %s/%s", moduleName, serviceName)

	// Setup local development if requested
	if libDepSetupLocal {
		cli.Info("Setting up local development...")
		resolver := resources.NewLibraryResolver(workspace)
		if err := resolver.SetupLocalDevelopment(ctx, svc); err != nil {
			cli.Warning("Failed to setup local development: %v", err)
			cli.Info("You may need to manually configure your build tools.")
		} else {
			cli.Info("Local development setup complete.")
		}
	} else {
		cli.Info("")
		cli.Info("To setup local development (go.mod replace, etc), run:")
		cli.Info("  codefly sync library-dependencies --service=%s --module=%s", serviceName, moduleName)
	}
	return nil
}

func init() {
	LibraryDependencyCmd.Flags().StringVar(&libDepService, "service", "", "Service name")
	LibraryDependencyCmd.Flags().StringVar(&libDepModule, "module", "", "Module name")
	LibraryDependencyCmd.Flags().StringVar(&libDepVersion, "version", "", "Version constraint (e.g., ^1.0.0)")
	LibraryDependencyCmd.Flags().StringSliceVar(&libDepLanguages, "languages", nil, "Language exports to use")
	LibraryDependencyCmd.Flags().BoolVar(&libDepSetupLocal, "setup-local", false, "Setup local development immediately")
}
