package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
)

func NewContext() (context.Context, func()) {
	ctx := context.Background()

	provider := wool.New(ctx, configurations.CLI.AsResource())

	provider.WithLogger(cli.GetLogger())

	ctx = provider.Inject(ctx)
	return ctx, provider.Done
}
