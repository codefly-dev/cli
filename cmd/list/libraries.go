package list

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/librarystore"
	"github.com/spf13/cobra"
)

var (
	listLibrariesJSON   bool
	listLibrariesRemote bool
)

// LibraryCmd lists the libraries declared in the current workspace.
var LibraryCmd = &cobra.Command{
	Use:   "libraries",
	Short: "List internal libraries available to workspace services",
	Long: `List the libraries under the workspace's libraries/ directory.

--remote additionally queries each library's configured store (network) for
its published versions.

Examples:
  codefly list libraries
  codefly list libraries --remote
  codefly list libraries --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return listLibraries(cmd)
	},
}

func init() {
	LibraryCmd.Flags().BoolVar(&listLibrariesJSON, "json", false, "Emit machine-readable JSON")
	LibraryCmd.Flags().BoolVar(&listLibrariesRemote, "remote", false, "Include each library's published versions from its configured store (network)")
}

type libraryListEntry struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Languages []string `json:"languages"`
	Published []string `json:"published,omitempty"`
}

func listLibraries(cmd *cobra.Command) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	libs, err := workspace.LoadLibraries(ctx)
	if err != nil {
		return fmt.Errorf("cannot load libraries: %w", err)
	}

	var cfg librarystore.StoreConfig
	if listLibrariesRemote {
		cfg, err = librarystore.LoadStoreConfig(workspace.Dir())
		if err != nil {
			return err
		}
	}

	entries := make([]libraryListEntry, 0, len(libs))
	for _, lib := range libs {
		languages := make([]string, 0, len(lib.Languages))
		for _, lang := range lib.Languages {
			languages = append(languages, lang.Name)
		}
		entry := libraryListEntry{Name: lib.Name, Version: lib.Version, Languages: languages}
		if listLibrariesRemote {
			for _, lang := range lib.Languages {
				language := librarystore.Language(lang.Name)
				store, err := librarystore.NewStoreFor(language, cfg)
				if err != nil {
					return err
				}
				versions, err := store.List(ctx, language, lib.Name)
				if err != nil {
					return fmt.Errorf("list published versions of %s (%s): %w", lib.Name, lang.Name, err)
				}
				for _, version := range versions {
					entry.Published = append(entry.Published, fmt.Sprintf("%s@%s", lang.Name, version))
				}
			}
		}
		entries = append(entries, entry)
	}

	if listLibrariesJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
	}

	if len(entries) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No libraries in workspace <%s>\n", workspace.Name)
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	header := "NAME\tVERSION\tLANGUAGES"
	if listLibrariesRemote {
		header += "\tPUBLISHED"
	}
	fmt.Fprintln(w, header)
	for _, e := range entries {
		row := fmt.Sprintf("%s\t%s\t%s", e.Name, e.Version, strings.Join(e.Languages, ","))
		if listLibrariesRemote {
			row += "\t" + strings.Join(e.Published, ",")
		}
		fmt.Fprintln(w, row)
	}
	return w.Flush()
}
