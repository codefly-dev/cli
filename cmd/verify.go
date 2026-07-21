package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/integrity"
	"github.com/spf13/cobra"
)

// VerifyCmd checks base-file integrity across the workspace's composed modules.
var VerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify base-file integrity of composed modules (consumers ADD, never modify the base)",
	Long: `Verify that every composed module still matches the base it synced from canonical.

For each module with a tools/base-manifest.json, re-hash the recorded base files
and fail on any modified or missing one. Files not in the manifest are legal
side-additions. To change a base file, promote the change upstream into canonical
or express it as a side-addition — never edit the synced copy.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return verifyCommand()
	},
}

func verifyCommand() error {
	ctx, done := common.NewContext()
	defer done()
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	report, verifyErr := integrity.VerifyBase(ctx, workspace)
	for _, module := range report.Modules {
		for _, allowed := range module.Allowed {
			cli.Info("  module <%s>: ALLOWED divergence (%s): %s", module.Module, allowed.Reason, allowed.Path)
		}
		if len(module.Omitted) > 0 {
			services := make([]string, 0, len(module.Omitted))
			total := 0
			for service, count := range module.Omitted {
				services = append(services, service)
				total += count
			}
			sort.Strings(services)
			cli.Info("  module <%s>: composed subset — skipped %d base files for non-composed service(s): %s", module.Module, total, strings.Join(services, ", "))
		}
		if module.Error == "" && len(module.Missing) == 0 && len(module.Modified) == 0 &&
			len(module.MissingRequiredAdditions) == 0 && len(module.InvalidRequiredAdditions) == 0 {
			cli.Info("✓ module <%s>: base intact (%d base files)", module.Module, module.Files)
			continue
		}
		if module.Error != "" {
			cli.Warning("✗ module <%s>: %s", module.Module, module.Error)
		}
		cli.Warning(
			"✗ module <%s>: %d modified, %d missing base, %d missing required addition, %d invalid required addition file(s)",
			module.Module,
			len(module.Modified),
			len(module.Missing),
			len(module.MissingRequiredAdditions),
			len(module.InvalidRequiredAdditions),
		)
		for _, relative := range module.Missing {
			cli.Warning("    MISSING  %s", relative)
		}
		for _, relative := range module.Modified {
			cli.Warning("    MODIFIED %s", relative)
		}
		for _, relative := range module.MissingRequiredAdditions {
			cli.Warning("    MISSING REQUIRED ADDITION %s", relative)
		}
		for _, relative := range module.InvalidRequiredAdditions {
			cli.Warning("    INVALID REQUIRED ADDITION %s", relative)
		}
	}
	if len(report.Modules) == 0 {
		cli.Info("base-integrity: no module carries a base-manifest.json — nothing to verify")
	} else if verifyErr == nil {
		cli.Info("base-integrity OK across %d module(s)", len(report.Modules))
	}
	return verifyErr
}
