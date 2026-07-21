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
)

func main() {
	if err := cmd.Execute(); err != nil {
		if !cmd.IsMachineReadableError(err) {
			cli.ErrorChain(err, "cannot execute command")
		}
		cli.Done()
		os.Exit(1)
	}
}
