package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/librarystore"
	"github.com/spf13/cobra"
)

var (
	destination     string
	installLanguage string
)

// LibraryCmd resolves a published library export through its configured
// store and, with --destination, installs it via the language's native
// package manager. It never vendors source: a consumer pins the immutable
// handle librarystore.Resolve returns.
var LibraryCmd = &cobra.Command{
	Use:   "library <name>@<constraint>",
	Short: "Install a published library export into the current workspace",
	Long: `Resolve the highest published version of a library export satisfying a
semantic version constraint, and print its durable install handle.

Examples:
  codefly install library authkit@^1.0.0 --language go
  codefly install library authkit@^1.0.0 --language go --destination ./services/api`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return installLibrary(cmd, args[0])
	},
}

func init() {
	LibraryCmd.Flags().StringVar(&destination, "destination", "", "Directory to run the native install command in (default: only print the resolved coordinates)")
	LibraryCmd.Flags().StringVar(&installLanguage, "language", "", "Language export to install (go, python, typescript)")
}

func installLibrary(cmd *cobra.Command, spec string) error {
	name, constraint, ok := strings.Cut(spec, "@")
	if !ok || name == "" || constraint == "" {
		return fmt.Errorf("library must be given as <name>@<constraint>, e.g. authkit@^1.0.0")
	}
	if installLanguage == "" {
		return fmt.Errorf("--language is required (go, python, typescript)")
	}
	language := librarystore.Language(installLanguage)

	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	cfg, err := librarystore.LoadStoreConfig(workspace.Dir())
	if err != nil {
		return err
	}
	store, err := librarystore.NewStoreFor(language, cfg)
	if err != nil {
		return err
	}

	published, err := store.Resolve(ctx, language, name, constraint)
	if err != nil {
		return fmt.Errorf("resolve %s@%s: %w", name, constraint, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s@%s\n", published.ImportPath, published.Version)
	fmt.Fprintf(out, "  install: %s\n", published.InstallHint)
	fmt.Fprintf(out, "  ref:     %s\n", published.Ref)
	fmt.Fprintf(out, "  digest:  %s\n", published.Digest)

	if destination == "" {
		return nil
	}
	return installInto(ctx, language, &published, destination)
}

func installInto(ctx context.Context, language librarystore.Language, published *librarystore.Published, destination string) error {
	switch language {
	case librarystore.LanguageGo:
		return runInDir(ctx, destination, "go", "get", fmt.Sprintf("%s@v%s", published.ImportPath, published.Version))
	case librarystore.LanguageTypeScript:
		return runInDir(ctx, destination, "npm", "install", fmt.Sprintf("%s@%s", published.ImportPath, published.Version))
	case librarystore.LanguagePython:
		return runInDir(ctx, destination, "pip", "install", fmt.Sprintf("git+%s@v%s", strings.TrimSuffix(published.Location, ".git"), published.Version))
	default:
		return fmt.Errorf("install: unsupported language %q", language)
	}
}

func runInDir(ctx context.Context, dir, name string, args ...string) error {
	//nolint:gosec // name/args are store-controlled coordinates (import path, version), never a shell.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
