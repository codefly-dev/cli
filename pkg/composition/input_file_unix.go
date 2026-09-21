//go:build unix

package composition

import (
	"context"
	"errors"
	"os"
	"syscall"
)

// Open before validating, but never wait for a FIFO peer. A path-level stat
// cannot protect against replacement between validation and the open syscall.
// Root-relative callers retain os.Root's containment checks.
func openInputFile(ctx context.Context, directory *os.Root, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var file *os.File
	var err error
	if directory == nil {
		file, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	} else {
		file, err = directory.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() > maxSelectionArtifactBytes) {
		err = errors.New("composition input must be a regular file within the artifact size limit")
	}
	if err = errors.Join(err, ctx.Err()); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
