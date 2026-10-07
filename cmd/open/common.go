package open

import (
	"context"
	"os/exec"
)

var editor string

// openInEditor hands a directory to the editor the --editor flag names.
func openInEditor(ctx context.Context, editor, directory string) error {
	return exec.CommandContext(ctx, editor, directory).Run()
}
