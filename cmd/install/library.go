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
	return installInto(ctx, language, &published, cfg, destination)
}

func installInto(ctx context.Context, language librarystore.Language, published *librarystore.Published, cfg librarystore.StoreConfig, destination string) error {
	switch language {
	case librarystore.LanguageGo:
		return runInDir(ctx, destination, nil, "go", "get", fmt.Sprintf("%s@v%s", published.ImportPath, published.Version))
	case librarystore.LanguageTypeScript:
		env, err := prepareNpmDestination(destination, cfg)
		if err != nil {
			return err
		}
		return runInDir(ctx, destination, env, "npm", "install", fmt.Sprintf("%s@%s", published.ImportPath, published.Version))
	case librarystore.LanguagePython:
		return runInDir(ctx, destination, nil, "pip", "install", librarystore.PipGitArgument(published.ImportPath, published.Version))
	default:
		return fmt.Errorf("install: unsupported language %q", language)
	}
}

// prepareNpmDestination wires destination up to authenticate against cfg's
// registry before npm ever runs there. A bare `npm install <pkg>@<version>`
// resolves against whatever registry destination's ambient npm config points
// at — by default registry.npmjs.org, not the store this package was
// actually published to. The scoped registry this store's language is
// configured for (GitHub Packages by default, which requires authentication
// even to read a public package) must be wired into destination the same way
// `codefly publish library` wires it for npm publish, or the install
// 404s/401s regardless of how the package was resolved.
func prepareNpmDestination(destination string, cfg librarystore.StoreConfig) ([]string, error) {
	if err := librarystore.WriteNpmrc(destination, cfg.NpmScope, cfg.NpmRegistry); err != nil {
		return nil, err
	}
	return []string{"NODE_AUTH_TOKEN=" + librarystore.NpmToken(cfg.NpmRegistry)}, nil
}

func runInDir(ctx context.Context, dir string, env []string, name string, args ...string) error {
	//nolint:gosec // name/args are store-controlled coordinates (import path, version), never a shell.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
