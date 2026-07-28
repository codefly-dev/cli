//go:build !darwin && !linux

package localservice

import (
	"fmt"
	"os"
)

func validateDefinitionFile(path string, _ int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("service definition permissions are %04o, want 0600", info.Mode().Perm())
	}
	return nil
}
