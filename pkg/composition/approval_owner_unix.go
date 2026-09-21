//go:build darwin || linux

package composition

import (
	"errors"
	"os"
	"syscall"
)

func trustedAuthorityOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && int64(stat.Uid) != int64(os.Geteuid())) {
		return errors.New("approval authority storage must be owned by root or the effective user")
	}
	return nil
}
