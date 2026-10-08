package show

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// SelectionCmd projects Core's resolved references without loading module services.
// It does not resolve configuration values or claim runtime/deployment evidence.
var SelectionCmd = &cobra.Command{
	Use:   "selection",
	Short: "Export composed module selections and their declaration owners as JSON",
	Long: `Resolve the current workspace through Core and export module references,
declaration-owner directories and product-owned environment names as JSON.
Local workspace imports are supported. Release imports without an installed host
resolver fail explicitly. No module services, configuration values or runtime
observations are collected. This is a point-in-time source report, not deployment
or complete input-identity evidence. Standard workspace loading rules apply.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		restore := cli.ProtectResultStream()
		defer restore()
		ctx, done := common.NewContext()
		defer done()
		wool.Get(ctx).WithLogger(cli.GetLogger())
		revision, _ := cmd.Flags().GetString("revision")
		if revision != "" {
			imports, _ := cmd.Flags().GetStringArray("import-revision")
			root, err := os.Getwd()
			if err != nil {
				return err
			}
			report, err := snapshotSelection(ctx, root, revision, imports)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
		}
		if cmd.Flags().Changed("import-revision") {
			return fmt.Errorf("import-revision requires revision")
		}
		ws, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot resolve workspace selection: %w", err)
		}
		report := selectionProjection(ws)
		attachSelectionReceipts(ctx, ws, &report)
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	},
}

type selectedModule struct {
	Name                 string               `json:"name"`
	Source               string               `json:"source"`
	Module               string               `json:"module"`
	Version              string               `json:"version"`
	DeclarationDirectory string               `json:"declaration_directory"`
	Resolution           *selectionResolution `json:"resolution,omitempty"`
}

// These are local receipt claims, not revalidated artifacts or runtime facts.
type selectionResolution struct {
	State            string                    `json:"state"`
	Record           string                    `json:"record"`
	Mode             string                    `json:"mode,omitempty"`
	Version          string                    `json:"version,omitempty"`
	Commit           string                    `json:"commit,omitempty"`
	ServiceOverrides bool                      `json:"service_overrides"`
	Previous         *previousSelectionReceipt `json:"previous,omitempty"`
}

// Previous identifies the mismatching receipt only; it is never an effective selection.
type previousSelectionReceipt struct {
	Source    string `json:"source"`
	Module    string `json:"module"`
	Requested string `json:"requested"`
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	Service   string `json:"service,omitempty"`
}

func attachSelectionReceipts(ctx context.Context, ws *resources.Workspace, report *selectionReport) {
	declared, declaredErr := composition.LoadModuleResolutions(ws.Dir())
	overlay, overlayErr := resources.LoadLocalOverlay(ctx, ws.Dir())
	dir := composition.NearestOverlayDir(ws.Dir())
	if dir == "" {
		dir = ws.Dir()
	}
	receipts, receiptErr := composition.LoadResolutionReceipts(dir)
	for i, ref := range ws.Modules {
		var directive *resources.ModuleResolveDirective
		if overlay != nil {
			directive = overlay.Resolve[ref.Name]
		}
		resolution := selectionReceipt(ref, directive, receipts[ref.Name], declared[ref.Name])
		if declaredErr != nil || overlayErr != nil || receiptErr != nil {
			resolution = &selectionResolution{State: "unavailable", Record: composition.ResolutionRecordName}
		}
		report.Modules[i].Resolution = resolution
	}
}

func selectionReceipt(ref *resources.ModuleReference, directive *resources.ModuleResolveDirective, receipt *composition.ResolutionReceipt, declared composition.WorkspaceResolution) *selectionResolution {
	r := &selectionResolution{State: "missing", Record: composition.ResolutionRecordName}
	if directive != nil {
		r.ServiceOverrides = len(directive.Services) > 0
	}
	if ref.PathOverride != nil || (directive != nil && (directive.Worktree != "" || (directive.Path != "" && directive.Path != receipt.ResolvedPath()))) {
		r.State = "local-override"
		return r
	}
	if receipt == nil {
		return r
	}
	mode := composition.ResolutionModeFor(directive, receipt, declared)
	if !receipt.Answers(ref, mode) || receipt.Service != "" {
		r.State = "different-request"
		r.Previous = &previousSelectionReceipt{Source: receipt.Source, Module: receipt.Module, Requested: receipt.Requested, Version: receipt.Version, Mode: string(receipt.Mode), Service: receipt.Service}
		return r
	}
	r.State, r.Mode, r.Version = "matching-record", string(mode), receipt.Version
	if regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(receipt.Commit) {
		r.Commit = receipt.Commit
	}
	return r
}

func init() {
	SelectionCmd.Flags().String("revision", "", "Inspect an immutable product commit instead of working-tree declarations")
	SelectionCmd.Flags().StringArray("import-revision", nil, "Pin an imported workspace as absolute-directory=commit (repeatable; requires revision)")
}

type selectedWorkspace struct {
	Revision        string `json:"revision,omitempty"`
	SourceMode      string `json:"source_mode,omitempty"`
	Name            string `json:"name"`
	Directory       string `json:"directory"`
	ParentDirectory string `json:"parent_directory,omitempty"`
	ManifestPath    string `json:"manifest_path,omitempty"`
	ManifestSHA256  string `json:"manifest_sha256,omitempty"`
}
type selectionReport struct {
	SchemaVersion int                 `json:"schema_version"`
	Workspace     string              `json:"workspace"`
	Directory     string              `json:"directory"`
	Modules       []selectedModule    `json:"modules"`
	Workspaces    []selectedWorkspace `json:"workspaces"`
	Environments  []string            `json:"environments"`
}

func selectionProjection(ws *resources.Workspace) selectionReport {
	report := selectionReport{SchemaVersion: 1, Workspace: ws.Name, Directory: ws.Dir(), Modules: []selectedModule{}, Workspaces: []selectedWorkspace{}, Environments: []string{}}
	for _, ref := range ws.Modules {
		report.Modules = append(report.Modules, selectedModule{Name: ref.Name, Source: ref.Source, Module: ref.Module, Version: ref.Version, DeclarationDirectory: ws.ModuleDeclarationDir(ref.Name)})
	}
	var visit func(*resources.Workspace, string)
	visit = func(current *resources.Workspace, parent string) {
		file, digest := current.DeclarationSource()
		report.Workspaces = append(report.Workspaces, selectedWorkspace{Name: current.Name, Directory: current.Dir(), ParentDirectory: parent, ManifestPath: file, ManifestSHA256: digest})
		for _, child := range current.ComposedWorkspaces() {
			visit(child, current.Dir())
		}
	}
	visit(ws, "")
	for _, env := range ws.Environments {
		report.Environments = append(report.Environments, env.Name)
	}
	return report
}
