//go:build darwin

package gitops

import (
	"os"

	"golang.org/x/sys/unix"
)

func exchangeDirectoriesAt(leftParent *os.File, left string, rightParent *os.File, right string) error {
	return unix.RenameatxNp(int(leftParent.Fd()), left, int(rightParent.Fd()), right, unix.RENAME_SWAP)
}
