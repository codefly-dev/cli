//go:build !unix

package composition

import (
	"context"
	"errors"
	"os"
)

func openInputFile(ctx context.Context, _ *os.Root, _ string) (*os.File, error) {
	return nil, errors.Join(ctx.Err(), errors.New("nonblocking composition input verification is unsupported on this platform"))
}
