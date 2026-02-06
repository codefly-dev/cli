package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
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

	ctx = provider.Inject(ctx)
	return ctx, provider.Done
}
