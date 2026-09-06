package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/librarystore"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	publishLibraryVersion   string
	publishLibraryLanguages []string
	publishLibraryDryRun    bool
)

// libraryCmd is `codefly publish library <name>`, a sibling of the
// patch/minor/major release flow above: it publishes a workspace library's
// language exports to the stores configured in workspace.codefly.yaml's
// libraries.publish block, via pkg/librarystore.
var libraryCmd = &cobra.Command{
	Use:   "library <name>",
	Short: "Publish a workspace library's language exports to their configured stores",
	Long: `Publish a workspace library (codefly add library) to the durable stores
configured under the workspace's libraries.publish block — a GitHub repository
tagged at the version for go/python, an npm-compatible registry for
typescript. Published versions are immutable: publishing the same version
twice fails.

Configure workspace.codefly.yaml:

  libraries:
    publish:
      go: {owner: codefly-dev}
      typescript: {registry: https://npm.pkg.github.com, scope: "@codefly-dev"}
      python: {owner: codefly-dev}

Examples:
  codefly publish library authkit --dry-run
  codefly publish library authkit --version 1.2.0
  codefly publish library authkit --language go,python`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return publishLibrary(cmd, args[0])
	},
}

func init() {
	libraryCmd.Flags().StringVar(&publishLibraryVersion, "version", "", "Version to publish (defaults to the library's version)")
	libraryCmd.Flags().StringSliceVar(&publishLibraryLanguages, "language", nil, "Languages to publish (defaults to all of the library's declared languages)")
	libraryCmd.Flags().BoolVar(&publishLibraryDryRun, "dry-run", false, "Show what would be published without publishing anything")
	Cmd.AddCommand(libraryCmd)
}

type publishedExport struct {
	Language    librarystore.Language
	Version     string
	ImportPath  string
	Ref         string
	Digest      string
	InstallHint string
}

func publishLibrary(cmd *cobra.Command, name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	lib, err := workspace.LoadLibraryFromName(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot load library %s: %w", name, err)
	}

	versionInput := publishLibraryVersion
	if versionInput == "" {
		versionInput = lib.Version
	}
	version, err := semver.NewVersion(strings.TrimPrefix(versionInput, "v"))
	if err != nil {
		return fmt.Errorf("%q is not a strict semantic version: %w", versionInput, err)
	}

	languageNames := publishLibraryLanguages
	if len(languageNames) == 0 {
		for _, lang := range lib.Languages {
			languageNames = append(languageNames, lang.Name)
		}
	}
	if len(languageNames) == 0 {
		return fmt.Errorf("library %s declares no language exports", name)
	}

	cfg, err := librarystore.LoadStoreConfig(workspace.Dir())
	if err != nil {
		return err
	}

	exports := make([]*resources.LanguageExport, 0, len(languageNames))
	for _, languageName := range languageNames {
		lang := lib.GetLanguage(languageName)
		if lang == nil {
			return fmt.Errorf("library %s has no %s export", name, languageName)
		}
		exports = append(exports, lang)
	}

	// Pre-flight every language before any language is published: a Go export
	// that publishes fine must not leave a Python export's stale version file
	// discovered only after the Go tag is already immutable.
	identities := make(map[librarystore.Language]string, len(exports))
	for _, lang := range exports {
		language := librarystore.Language(lang.Name)
		importPath, _, err := librarystore.PreviewIdentity(language, cfg, lib.Name, version.String())
		if err != nil {
			return err
		}
		identities[language] = importPath
		if err := preflightLanguageExport(lib, lang, importPath, version.String()); err != nil {
			return err
		}
	}

	if publishLibraryDryRun {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "LANGUAGE\tIMPORT PATH\tINSTALL HINT")
		for _, lang := range exports {
			language := librarystore.Language(lang.Name)
			_, hint, err := librarystore.PreviewIdentity(language, cfg, lib.Name, version.String())
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", language, identities[language], hint)
		}
		return w.Flush()
	}

	published := make([]publishedExport, 0, len(exports))
	for _, lang := range exports {
		language := librarystore.Language(lang.Name)
		store, err := librarystore.NewStoreFor(language, cfg)
		if err != nil {
			return reportPartialPublish(published, err)
		}
		result, err := store.Publish(ctx, lib.LanguagePath(lang), librarystore.Coordinates{
			Language: language,
			Name:     lib.Name,
			Version:  version.String(),
		})
		if err != nil {
			return reportPartialPublish(published, fmt.Errorf("publish %s export: %w", language, err))
		}
		published = append(published, publishedExport{
			Language: language, Version: result.Version, ImportPath: result.ImportPath,
			Ref: result.Ref, Digest: result.Digest, InstallHint: result.InstallHint,
		})
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LANGUAGE\tVERSION\tIMPORT PATH\tREF/DIGEST\tINSTALL HINT")
	for _, p := range published {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Language, p.Version, p.ImportPath, p.Digest, p.InstallHint)
	}
	return w.Flush()
}

// reportPartialPublish surfaces which languages already published — those
// versions are immutable and this command never rolls them back — alongside
// the error that stopped the remaining languages.
func reportPartialPublish(published []publishedExport, cause error) error {
	if len(published) == 0 {
		return cause
	}
	done := make([]string, len(published))
	for i, p := range published {
		done[i] = string(p.Language)
	}
	return fmt.Errorf("%w (already published and immutable: %s)", cause, strings.Join(done, ", "))
}

// preflightLanguageExport checks what store.Publish would otherwise discover
// only after touching the network: the export directory exists, its declared
// export identity (when the manifest carries one) matches what this store
// would publish under, and its language-specific version file agrees with the
// version being published.
func preflightLanguageExport(lib *resources.Library, lang *resources.LanguageExport, expectedImportPath, version string) error {
	dir := lib.LanguagePath(lang)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("library %s: %s export directory %s does not exist", lib.Name, lang.Name, dir)
	}
	if len(lang.Exports) > 0 && lang.Exports[0] != expectedImportPath {
		return fmt.Errorf("library %s: %s export declares %q but the configured store publishes under %q",
			lib.Name, lang.Name, lang.Exports[0], expectedImportPath)
	}

	switch lang.Name {
	case string(librarystore.LanguageGo):
		return nil // go.mod carries no separate version field; librarystore validates the module path at publish time.
	case string(librarystore.LanguageTypeScript):
		path := filepath.Join(dir, "package.json")
		declared, err := readJSONVersionField(path)
		if err != nil {
			return fmt.Errorf("library %s: %w", lib.Name, err)
		}
		if declared != version {
			return fmt.Errorf("bump %s to %s", path, version)
		}
	case string(librarystore.LanguagePython):
		path := filepath.Join(dir, "pyproject.toml")
		declared, err := readPyprojectVersion(path)
		if err != nil {
			return fmt.Errorf("library %s: %w", lib.Name, err)
		}
		if declared != version {
			return fmt.Errorf("bump %s to %s", path, version)
		}
	}
	return nil
}

func readJSONVersionField(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Version, nil
}

// readPyprojectVersion extracts `version = "X.Y.Z"` from the [project] table
// of a pyproject.toml. A hand-written line scan, not a TOML parser: the
// version field is the only value this command needs, and codefly does not
// otherwise depend on a TOML library.
func readPyprojectVersion(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	inProject := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inProject = trimmed == "[project]"
			continue
		}
		if !inProject {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "version" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`), nil
	}
	return "", fmt.Errorf("%s: no version field in [project]", path)
}
