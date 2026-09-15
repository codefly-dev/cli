package test

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/run"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/spf13/cobra"
)

// SolutionCmd tests a solution as a unit from its root. It resolves the same
// service-entry `run solution` boots and delegates to the test-service path, so
// a solution is tested through exactly the orchestration that runs it — no
// second sequencing engine, and the solution-derived inputs
// (CODEFLY__API_CONSUMES, registration secrets) reach the origin either way.
var SolutionCmd = &cobra.Command{
	Use:   "solution",
	Short: "Test a solution: run its service-entry's tests against the full dependency graph",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			done()
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		// Pull composed modules that resolve to a pinned artifact into the local
		// cache and point the overlay at them, so the delegated test below loads
		// them as local checkouts instead of erroring on an unfetched coordinate.
		if err = composition.MaterializePinnedModules(ctx, workspace); err != nil {
			done()
			return err
		}
		// Reload so the overlay materialize just wrote is in effect: composed
		// pinned modules now resolve to their cache checkout, which the
		// service-entry scan may need to load.
		workspace, err = common.LoadWorkspace(ctx)
		if err != nil {
			done()
			return fmt.Errorf("cannot reload workspace: %w", err)
		}
		entry, err := run.ResolveSolutionEntry(ctx, workspace)
		done()
		if err != nil {
			return err
		}
		// The pins are declared resolved: materialization above already ran
		// unconditionally, so letting the delegate run it again would re-attempt
		// any pull that warned there — a second round trip and a duplicate
		// warning, for a request nothing has changed since.
		pinsAlreadyResolved = true
		defer func() { pinsAlreadyResolved = false }()
		return testServiceCommand(cmd, []string{entry})
	},
}

func init() {
	// Solution-facing subset of the test flags, bound to the same package vars
	// testServiceCommand reads.
	SolutionCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	SolutionCmd.Flags().StringVar(&testFixture, "fixture", "", "Fixture override (defaults to the selected Codefly environment)")
	SolutionCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY)")

	// Test filter flags — forwarded to the agent's Test RPC, same as `test service`.
	SolutionCmd.Flags().StringVar(&testTarget, "target", "", "Package/directory scope (Go: ./pkg/foo, Python: tests/unit)")
	SolutionCmd.Flags().StringSliceVarP(&testFilters, "filter", "k", nil, "Name regex pattern (repeatable; OR-combined). -k mirrors pytest")
	SolutionCmd.Flags().StringVar(&testSuite, "suite", "", "Named suite: unit (default), integration, e2e, smoke")
	SolutionCmd.Flags().StringVar(&testTimeout, "timeout", "", "Per-test timeout, e.g. 30s")
	SolutionCmd.Flags().BoolVarP(&testVerbose, "verbose", "v", false, "Verbose runner output")

	bindSharedTestFlags(SolutionCmd)
}
