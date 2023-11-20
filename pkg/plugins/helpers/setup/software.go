package setup

import (
	"fmt"
	"os/exec"
)

type MissingSoftwareError struct {
	Software string
}

func (m MissingSoftwareError) Error() string {
	return fmt.Sprintf("missing software: %s", m.Software)
}

func NewMissingSoftwareError(software string) MissingSoftwareError {
	return MissingSoftwareError{Software: software}
}

func Has(exe string) bool {
	_, err := exec.LookPath(exe)
	return err == nil
}
