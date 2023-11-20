package api

import (
	"os"
	"os/exec"

	"github.com/codefly-dev/cli/pkg/plugins/helpers/setup"
)

type Proto struct {
	Dir string
}

func (g *Proto) Generate() error {
	if !setup.Has("buf") {
		return setup.NewMissingSoftwareError("buf")
	}
	cmd := exec.Command("buf", "mod", "update")
	cmd.Dir = g.Dir
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd = exec.Command("buf", "generate")
	cmd.Dir = g.Dir
	cmd.Env = os.Environ()
	return cmd.Run()
}
