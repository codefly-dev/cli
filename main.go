package main

import (
	"os"

	"github.com/codefly-dev/cli/cmd"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/timings"

	// Registers the companion index this CLI carries: core compiles no image version in.
	_ "github.com/codefly-dev/cli/pkg/companions"
)

func main() {
	err := cmd.Execute()
	timings.Report(os.Stderr)
	if err != nil {
		if cmd.ShouldRenderError(err) {
			cli.ErrorChain(err, "cannot execute command")
		}
		cli.Done()
		os.Exit(cmd.ExitCode(err))
	}
}
