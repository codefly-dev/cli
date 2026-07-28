//go:build darwin || linux

package localservice

import (
	"fmt"
	"os"
	"syscall"
)

func validateDefinitionFile(path string, uid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("service definition permissions are %04o, want 0600", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid {
		return fmt.Errorf("service definition is not owned by the current user")
	}
	return nil
}
