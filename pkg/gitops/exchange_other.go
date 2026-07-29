//go:build !darwin && !linux

package gitops

import "fmt"

func exchangeDirectories(_, _ string) error {
	return fmt.Errorf("atomic directory exchange is not supported on this platform")
}
