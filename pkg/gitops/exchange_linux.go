//go:build linux

package gitops

import (
	"os"

	"golang.org/x/sys/unix"
)

func exchangeDirectoriesAt(leftParent *os.File, left string, rightParent *os.File, right string) error {
	return unix.Renameat2(int(leftParent.Fd()), left, int(rightParent.Fd()), right, unix.RENAME_EXCHANGE)
}
