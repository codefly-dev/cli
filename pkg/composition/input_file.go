package composition

import (
	"context"
	"os"
)

// OpenInputFile returns a caller-owned read-only descriptor after nonblocking
// open and bounded regular-file validation. Callers may impose smaller limits.
func OpenInputFile(ctx context.Context, path string) (*os.File, error) {
	return openInputFile(ctx, nil, path)
}
