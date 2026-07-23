// Package upgrade implements the `codefly upgrade` subtree.
//
// `codefly upgrade service <name>` runs the agent's Builder.Upgrade
// RPC, which applies semver-safe (patch+minor) bumps. --major opts in
// to breaking bumps. --dry-run reports without writing the lockfile.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

var (
	jsonOut      bool
	includeMajor bool
	dryRun       bool
	only         []string
)

// ServiceCmd implements `codefly upgrade service [name]`.
var ServiceCmd = &cobra.Command{
	Use:   "service [name]",
	Short: "Apply semver-safe dependency upgrades to one service",
	Long: `Run the service agent's Builder.Upgrade RPC. Defaults to
patch+minor (semver-safe) bumps. Pass --major to allow breaking
upgrades. Pass --dry-run to preview without writing the lockfile.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		defer services.ClearAgents()

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}
		resp, err := upgradeService(ctx, workspace, module, service)
		if err != nil {
			return fmt.Errorf("upgrade failed: %w", err)
		}

		identity, err := service.Identity()
		if err != nil {
			return fmt.Errorf("cannot get service identity: %w", err)
		}

		if jsonOut {
			if err := emitJSON(resp); err != nil {
				return err
			}
		} else {
			emitTable(identity, resp)
		}
		return ctx.Err()
	},
}

func upgradeService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*builderv0.UpgradeResponse, error) {
	if workspace == nil {
		return nil, fmt.Errorf("workspace is nil")
	}
	if module == nil {
		return nil, fmt.Errorf("module is nil")
	}
	if service == nil {
		return nil, fmt.Errorf("service is nil")
	}
	w := wool.Get(ctx).In("upgradeService", wool.NameField(service.Name))
	instance, err := services.Load(ctx, workspace, module, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service")
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, w.Wrapf(err, "builder load")
	}
	return instance.Builder.Upgrade(ctx, &builderv0.UpgradeRequest{
		IncludeMajor: includeMajor,
		DryRun:       dryRun,
		Only:         only,
	})
}

func emitJSON(r *builderv0.UpgradeResponse) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("cannot encode upgrade response: %w", err)
	}
	return nil
}

func emitTable(identity *resources.ServiceIdentity, r *builderv0.UpgradeResponse) {
	if r == nil {
		fmt.Println("(no response)")
		return
	}
	cli.Header(1, "Upgrade: %s/%s", identity.Module, identity.Name)
	state := r.GetState()
	if state != nil {
		fmt.Printf("  state: %s  %s\n", state.State.String(), state.Message)
	}
	if len(r.Changes) == 0 {
		fmt.Println("  ✓ nothing to upgrade")
	} else {
		fmt.Printf("\n  Changes (%d):\n", len(r.Changes))
		for _, c := range r.Changes {
			fmt.Printf("    %s  %s → %s\n", c.Package, c.From, c.To)
		}
	}
	if r.LockfileDiff != "" {
		fmt.Printf("\n  Lockfile diff:\n%s\n", indent(r.LockfileDiff, "    "))
	}
}

// indent prepends prefix to every non-empty line of s. Used to render
// the lockfile diff in the table layout.
func indent(s, prefix string) string {
	var b strings.Builder
	b.Grow(len(s) + len(prefix)*8)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			b.WriteString(prefix)
			b.WriteString(s[start : i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		b.WriteString(prefix)
		b.WriteString(s[start:])
	}
	return b.String()
}

func init() {
	ServiceCmd.Flags().BoolVar(&jsonOut, "json", false, "Emit raw JSON instead of a table")
	ServiceCmd.Flags().BoolVar(&includeMajor, "major", false, "Allow major-version bumps (breaking)")
	ServiceCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview changes without writing the lockfile")
	ServiceCmd.Flags().StringSliceVar(&only, "only", nil, "Restrict upgrades to these packages (comma-separated)")
}
