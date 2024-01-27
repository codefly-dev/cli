package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
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

	provider := wool.New(ctx, configurations.CLI.AsResource())

	provider.WithLogger(cli.GetLogger())

	ctx = provider.Inject(ctx)
	return ctx, provider.Done
}
