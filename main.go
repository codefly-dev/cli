package main

import (
	"os"

	"github.com/codefly-dev/cli/cmd"
	"github.com/codefly-dev/cli/pkg/cli"
)

func main() {
	if err := cmd.Execute(); err != nil {
		if cmd.ShouldRenderError(err) {
			cli.ErrorChain(err, "cannot execute command")
		}
		cli.Done()
		os.Exit(cmd.ExitCode(err))
	}
}
