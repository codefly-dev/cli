//go:build (!darwin && !linux) || (darwin && !cgo)

package composition

import (
	"errors"
	"os"
)

func validateAuthorityAccess(_ *os.File) error {
	return errors.New("approval authority access verification is unsupported on this platform/build")
}

func validatePrivateInputAccess(_ *os.File) error {
	return errors.New("private approved input access verification is unsupported on this platform/build")
}
