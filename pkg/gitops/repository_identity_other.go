//go:build !darwin && !linux

package gitops

import "os"

// fileIdentity is unavailable here, so a local promotion is refused rather
// than bound by path alone.
func fileIdentity(os.FileInfo) (device, inode uint64, ok bool) { return 0, 0, false }
