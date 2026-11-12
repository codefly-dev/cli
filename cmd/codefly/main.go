// Command codefly is the canonical versioned installation entry point:
//
//	go install github.com/codefly-dev/cli/cmd/codefly@<version>
//
// Keeping the executable in a directory named codefly makes Go install the
// expected binary name without workflow-local rename steps.
package main

import (
	"os"

	"github.com/codefly-dev/cli/cmd"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/timings"
)

func main() {
	// The released binary is built from here, so the CODEFLY_TIMINGS report is
	// printed here too, exactly as the repository root's main prints it.
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
