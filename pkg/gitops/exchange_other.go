//go:build !darwin && !linux

package gitops

import (
	"fmt"
	"os"
)

func exchangeDirectoriesAt(_ *os.File, _ string, _ *os.File, _ string) error {
	return fmt.Errorf("atomic directory exchange is not supported on this platform")
}
