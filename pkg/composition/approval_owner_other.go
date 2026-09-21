//go:build !darwin && !linux

package composition

import (
	"errors"
	"os"
)

func trustedAuthorityOwner(_ os.FileInfo) error {
	return errors.New("approval authority storage ownership verification is unsupported on this platform")
}
