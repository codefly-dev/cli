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
	libraryLanguages []string
	libraryGitRemote string
	libraryGitBranch string
)

// LibraryCmd represents the add library command
var LibraryCmd = &cobra.Command{
	Use:   "library [name]",
	Short: "Add an internal shared library",
	Long: `Add an internal shared library to the workspace.

Libraries are internal shared code that can be used by multiple services.
They support versioning through git tags and can be linked as git submodules.

Examples:
  # Create a new local library with Go support
  codefly add library shared-models --languages=go

  # Create a library with multiple language support
  codefly add library utils --languages=go,python

  # Add an external library as a git submodule
  codefly add library shared-models --git=git@github.com:myorg/shared-models.git
`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		addLibrary(name)
	},
}

func addLibrary(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)

	// Check if library already exists
	_, err := workspace.LoadLibraryFromName(ctx, name)
	if err == nil {
		cli.Error("Library <%s> already exists", name)
		os.Exit(1)
	}

	// Handle git submodule case
	if libraryGitRemote != "" {
		addLibraryAsSubmodule(workspace, name)
		return
	}

	// Validate languages
	if len(libraryLanguages) == 0 {
		libraryLanguages = []string{"go"} // Default to Go
	}

	confirm := models.Confirm(ctx,
		fmt.Sprintf("Add library <%s> with languages %v to workspace <%s>?",
			name, libraryLanguages, workspace.Name),
		true)
	if !confirm {
		cli.Header(2, "Cancelled.")
		os.Exit(0)
	}

	lib, err := workspace.CreateLibrary(ctx, name, libraryLanguages)
	if err != nil {
		cli.ExitOnError(err, "cannot create library")
	}

	cli.Header(2, "Library <%s> created at %s", lib.Name, lib.Dir())
	cli.Info("Add your code to the language directories:")
	for _, lang := range lib.Languages {
		cli.Info("  - %s: %s", lang.Name, lib.LanguagePath(lang))
	}
	cli.Info("")
	cli.Info("Then add it as a dependency to your services:")
	cli.Info("  codefly add library-dependency %s --service=<service> --module=<module>", name)
}

func addLibraryAsSubmodule(workspace *resources.Workspace, name string) {
	ctx, done := common.NewContext()
	defer done()

	branch := libraryGitBranch
	if branch == "" {
		branch = "main"
	}

	confirm := models.Confirm(ctx,
		fmt.Sprintf("Add library <%s> from %s as git submodule?", name, libraryGitRemote),
		true)
	if !confirm {
		cli.Header(2, "Cancelled.")
		os.Exit(0)
	}

	lib, err := workspace.AddLibraryAsSubmodule(ctx, name, libraryGitRemote, branch)
	if err != nil {
		cli.ExitOnError(err, "cannot add library as submodule")
	}

	cli.Header(2, "Library <%s> added as git submodule", lib.Name)
	cli.Info("Version: %s", lib.Version)
	cli.Info("Path: %s", lib.Dir())
}

func init() {
	LibraryCmd.Flags().StringSliceVarP(&libraryLanguages, "languages", "l", nil,
		"Languages to support (e.g., go,python,typescript)")
	LibraryCmd.Flags().StringVar(&libraryGitRemote, "git", "",
		"Git remote URL to add as submodule")
	LibraryCmd.Flags().StringVar(&libraryGitBranch, "branch", "main",
		"Git branch for submodule")
}
