package test

import (
	"errors"
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
//
// It also runs the composition tests the composed modules contribute, so a
// module can ship a test that every solution composing it runs.
var SolutionCmd = &cobra.Command{
	Use:   "solution",
	Short: "Test a solution: its service-entry's tests, plus the composition tests its composed modules contribute",
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
		// Run the contributed composition tests here, before done() closes the
		// context the delegation no longer shares. Their failure does not stop the
		// entry's own tests: a solution whose composed module ships a broken suite
		// still needs to report whether the solution itself works, so both results
		// are collected and joined below.
		contributedErr := runContributedCompositionTests(ctx, workspace)
		entry, err := run.ResolveSolutionEntry(ctx, workspace)
		done()
		if err != nil {
			return errors.Join(contributedErr, err)
		}
		// The pins are declared resolved: materialization above already ran
		// unconditionally, so letting the delegate run it again would re-attempt
		// any pull that warned there — a second round trip and a duplicate
		// warning, for a request nothing has changed since.
		pinsAlreadyResolved = true
		defer func() { pinsAlreadyResolved = false }()
		return errors.Join(contributedErr, testServiceCommand(cmd, []string{entry}))
	},
}

func init() {
	// A solution's entry is a service and this command runs the same path, so it
	// takes the same flags — registered once, never hand-listed per verb.
	bindSharedTestFlags(SolutionCmd)
}
