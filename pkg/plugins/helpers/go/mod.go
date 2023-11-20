package golang

import (
	"os/exec"

	"github.com/codefly-dev/cli/pkg/plugins"
)

func (g *Go) ModTidy(logger *plugins.PluginLogger) error {
	logger.Info("Tidying go modules...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = g.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return logger.Wrapf(err, "cannot run go mod tidy: %s", string(out))
	}
	return nil
}

func (g *Go) Update(logger *plugins.PluginLogger) error {
	logger.Info("Updating go modules...")
	cmd := exec.Command("go", "get", "-u", "./...")
	cmd.Dir = g.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return logger.Wrapf(err, "cannot run go get -u: %s", string(out))
	}
	return nil
}
