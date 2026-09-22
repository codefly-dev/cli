package override

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// ServiceCmd edits the machine-local overlay so that one service of a composed
// module comes from somewhere else, leaving the rest of the module — and every
// committed file — untouched.
var ServiceCmd = &cobra.Command{
	Use:   "service <module>/<service>",
	Short: "Run one service of a composed module from a checkout, a worktree, or another version",
	Long: `Override where a single service of a composed module comes from on this machine.

The override is written to ` + resources.LocalOverlayConfigurationName + `, which is gitignored and never
committed: the workspace's committed configuration keeps naming the module as a
whole, and only this machine runs the service from somewhere else. The rest of
the module is unaffected.

Exactly one of --path, --worktree, or --version selects the source:

  --path      a directory holding the service, for editing it in place
  --worktree  <owner/repo>@<ref>, matched against your local checkouts; the
              service is taken from that checkout's copy of the module
  --version   the module package at that version, pulled under the workspace's
              module-trust policy; the service is taken from it

The overriding directory must be the same service the module composed: same
name, same agent name, and at least the endpoints the module declares. Its agent
version may differ — running a service at a different agent version is a reason
to override it.

This works on every workspace layout. On a flat (single-module) workspace the
module name is the workspace's own name, and ` + "`codefly run service --service-path`" + `
remains the per-run spelling for the service you are launching.`,
	Example: `  # Run saas/accounts from a local checkout
  codefly override service saas/accounts --path ~/module-saas-starter/module/services/accounts

  # Take it from the checkout you have on a feature branch
  codefly override service saas/auth-gateway --worktree codefly-dev/module-saas-starter@feat/gateway-x

  # Run it at a published module version
  codefly override service saas/telemetry --version 0.0.66

  # Go back to the module's own copy
  codefly override service saas/accounts --clear`,
	Args: cobra.ExactArgs(1),
	RunE: runOverrideService,
}

var (
	servicePath     string
	serviceWorktree string
	serviceVersion  string
	serviceClear    bool
)

func init() {
	ServiceCmd.Flags().StringVar(&servicePath, "path", "", "directory holding the service")
	ServiceCmd.Flags().StringVar(&serviceWorktree, "worktree", "", "<owner/repo>@<ref> of a local checkout to take the service from")
	ServiceCmd.Flags().StringVar(&serviceVersion, "version", "", "module package version to take the service from")
	ServiceCmd.Flags().BoolVar(&serviceClear, "clear", false, "remove the override and go back to the module's own copy")
}

func runOverrideService(_ *cobra.Command, args []string) error {
	ctx, done := common.NewContext()
	defer done()

	module, service, err := parseServiceCoordinate(args[0])
	if err != nil {
		return err
	}
	directive, err := selectedDirective()
	if err != nil {
		return err
	}

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	if !workspace.ExistsModule(module) {
		return fmt.Errorf("workspace %s composes no module <%s>; it composes %s", workspace.Name, module, strings.Join(composedModules(workspace), ", "))
	}

	// The overlay core reads is the nearest one up the tree, so edits go back to
	// that same file. Writing a fresh workspace-local overlay instead would
	// shadow an ancestor's entries wholesale rather than add one to them.
	writeDir := workspace.Dir()
	if dir := composition.NearestOverlayDir(workspace.Dir()); dir != "" {
		writeDir = dir
	}
	// Load, mutate and save is one critical section. Every writer saves the whole
	// file, so a concurrent `codefly run` materializing pinned modules would
	// otherwise save its own snapshot over this edit, or lose its materialized
	// paths to it — whichever landed second would win the entire map.
	return composition.WithOverlayLock(writeDir, func() error {
		return editServiceOverride(ctx, workspace.Dir(), writeDir, module, service, directive)
	})
}

// editServiceOverride performs the overlay's read-decide-write cycle, holding
// the overlay lock.
func editServiceOverride(ctx context.Context, workspaceDir, writeDir, module, service string, directive *resources.ServiceResolveDirective) error {
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		return fmt.Errorf("cannot load local overlay: %w", err)
	}
	if overlay == nil {
		overlay = &resources.LocalOverlay{}
	}
	if overlay.Resolve == nil {
		overlay.Resolve = map[string]*resources.ModuleResolveDirective{}
	}

	if serviceClear {
		if entry := overlay.Resolve[module]; entry == nil || entry.Services[service] == nil {
			// Nothing to clear. Saving anyway would create a codefly.local.yaml —
			// and, on a workspace that had none, a stray untracked file — for a
			// command that changed nothing.
			cli.Header(2, "Service <%s/%s> has no override.", module, service)
			return nil
		}
		composition.ClearServiceOverride(overlay, module, service)
		if err = saveOverlay(ctx, writeDir, overlay); err != nil {
			return err
		}
		cli.Header(2, "Service <%s/%s> is back to the module's own copy.", module, service)
		return nil
	}

	entry := overlay.Resolve[module]
	if entry == nil {
		entry = &resources.ModuleResolveDirective{}
		overlay.Resolve[module] = entry
	}
	if entry.Services == nil {
		entry.Services = map[string]*resources.ServiceResolveDirective{}
	}
	entry.Services[service] = directive
	if err = saveOverlay(ctx, writeDir, overlay); err != nil {
		return err
	}

	reportErr := reportOverride(ctx, workspaceDir, module, service)
	if reportErr == nil {
		return nil
	}
	// An override that does not resolve is not one to leave written: every later
	// command reads this overlay, so a rejected directive would break the
	// workspace until someone found and cleared it. Either the command leaves a
	// working override or it leaves what it found.
	composition.ClearServiceOverride(overlay, module, service)
	if err = saveOverlay(ctx, writeDir, overlay); err != nil {
		return errors.Join(reportErr, fmt.Errorf("cannot roll back local overlay: %w", err))
	}
	return reportErr
}

// saveOverlay writes the overlay and keeps it out of git. The overlay records
// machine-local absolute paths, so a workspace holding one without the ignore
// rule hands it to the next `git add -A`. The rule is (re)asserted on every
// save rather than only on creation, because an overlay this command did not
// create may never have been ignored either.
func saveOverlay(ctx context.Context, writeDir string, overlay *resources.LocalOverlay) error {
	if err := resources.SaveLocalOverlay(ctx, writeDir, overlay); err != nil {
		return fmt.Errorf("cannot save local overlay: %w", err)
	}
	if err := composition.EnsureOverlayIgnored(writeDir); err != nil {
		return fmt.Errorf("cannot gitignore %s: %w", resources.LocalOverlayConfigurationName, err)
	}
	return nil
}

// reportOverride prints where the override just written actually lands, by
// asking the resolver the same question `codefly run` will. Reloading the
// workspace is what makes it the resolver's answer rather than an echo of the
// flag: a worktree that matches no checkout fails here, at the moment the user
// set it, instead of at the next run.
func reportOverride(ctx context.Context, dir, module, service string) error {
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		return fmt.Errorf("cannot reload workspace: %w", err)
	}
	ref, err := moduleReference(workspace, module)
	if err != nil {
		return err
	}
	resolution, err := workspace.ResolveModule(ctx, ref)
	if err != nil {
		return err
	}
	resolved := resolution.Services[service]
	if resolved.Kind == resources.ResolutionPinned {
		cli.Header(2, "Service <%s/%s> resolves to module package %s at version %s; `codefly run` pulls it.", module, service, resolved.Source, resolved.Version)
		return nil
	}
	cli.Header(2, "Service <%s/%s> resolves to %s.", module, service, resolved.Dir)
	return nil
}

// parseServiceCoordinate splits "<module>/<service>". The module half is
// required: a bare service name would be ambiguous in exactly the composed
// workspaces this command exists for.
func parseServiceCoordinate(coordinate string) (string, string, error) {
	module, service, found := strings.Cut(coordinate, "/")
	if !found || module == "" || service == "" {
		return "", "", fmt.Errorf("service coordinate %q must be <module>/<service>", coordinate)
	}
	return module, service, nil
}

// selectedDirective turns the flags into the one directive the overlay records,
// refusing a call that selects none or more than one — the same rule the overlay
// schema itself enforces, applied before anything is written.
func selectedDirective() (*resources.ServiceResolveDirective, error) {
	var selected []string
	directive := &resources.ServiceResolveDirective{}
	if servicePath != "" {
		absolute, err := filepath.Abs(servicePath)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve --path %q: %w", servicePath, err)
		}
		directive.Path = absolute
		selected = append(selected, "--path")
	}
	if serviceWorktree != "" {
		repo, ref, ok := strings.Cut(serviceWorktree, "@")
		if !ok || repo == "" || ref == "" {
			return nil, fmt.Errorf("--worktree %q must be <owner/repo>@<ref>", serviceWorktree)
		}
		directive.Worktree = serviceWorktree
		selected = append(selected, "--worktree")
	}
	if serviceVersion != "" {
		directive.Version = serviceVersion
		selected = append(selected, "--version")
	}
	if serviceClear {
		selected = append(selected, "--clear")
	}
	switch len(selected) {
	case 1:
		return directive, nil
	case 0:
		return nil, fmt.Errorf("give one of --path, --worktree, --version, or --clear")
	default:
		return nil, fmt.Errorf("%s select different sources; give exactly one", strings.Join(selected, " and "))
	}
}

func moduleReference(workspace *resources.Workspace, module string) (*resources.ModuleReference, error) {
	for _, ref := range workspace.Modules {
		if ref.Name == module {
			return ref, nil
		}
	}
	return nil, fmt.Errorf("workspace %s composes no module <%s>", workspace.Name, module)
}

func composedModules(workspace *resources.Workspace) []string {
	names := make([]string, len(workspace.Modules))
	for i, ref := range workspace.Modules {
		names[i] = ref.Name
	}
	return names
}
