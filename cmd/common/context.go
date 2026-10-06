package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/agents"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

type cancelContextKey struct{}

// Cancel retrieves the cancel function from the context
func Cancel(ctx context.Context) {
	cancel, ok := ctx.Value(cancelContextKey{}).(func())
	if ok {
		cancel()
	}
}

func NewContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())

	// Store the cancel function in the context
	ctx = context.WithValue(ctx, cancelContextKey{}, cancel)

	provider := wool.New(ctx, resources.CLI.AsResource())

	provider.WithLogger(cli.GetLogger())

	// Enable file logging to ~/.codefly/logs/<date>.log
	cli.EnableFileLogging()
	if fl := cli.GetFileLogger(); fl != nil {
		agents.AddProcessor(fl)
	}

	ctx = provider.Inject(ctx)

	// A `workspaces:` entry naming a release has no directory until a host
	// produces one, and core refuses with "requires host resolution" when no
	// resolver is registered. The root command's PersistentPreRunE registers one
	// on `cmd.Context()` -- but 121 of this CLI's RunE bodies discard the
	// *cobra.Command entirely (`func(_ *cobra.Command, args []string)`) and
	// start from here instead, so the root's registration never reached them.
	// `codefly doctor workspace` reads cmd.Context() and worked; `codefly deploy
	// gitops render` does not and died on exactly the error the resolver exists
	// to prevent.
	//
	// This is the one function every command's context is born in, so it is
	// where the resolver belongs. The root's registration stays for the 42 sites
	// that do use cmd.Context().
	ctx = composition.WithWorkspaceResolver(ctx)

	return ctx, provider.Done
}
