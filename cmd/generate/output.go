package generate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/shared"
)

// solveOutputDirectory resolves and creates a generator-owned destination.
// Unlike an input path, an output directory is expected not to exist on the
// first invocation; requiring callers to pre-create it defeats the CLI's
// generation contract and breaks headless automation.
func solveOutputDirectory(ctx context.Context, destination string) (string, error) {
	if strings.TrimSpace(destination) == "" {
		return "", fmt.Errorf("destination is required")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve destination: %w", err)
	}
	if _, err := shared.CheckDirectoryOrCreate(ctx, abs); err != nil {
		return "", fmt.Errorf("create destination: %w", err)
	}
	return shared.SolvePath(abs)
}
