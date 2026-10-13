//go:build darwin || linux

package gitops

import (
	"os"
	"syscall"
)

// fileIdentity identifies the directory even through aliases. Replacing
// the directory changes its identity; renaming the original preserves it.
func fileIdentity(info os.FileInfo) (device, inode uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true //nolint:gosec,unconvert // G115: a device number is never negative; Dev is int32 on darwin
}
