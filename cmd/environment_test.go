package cmd

// The command tree reads the surrounding run while it is built (the default of
// --runtime-context is the run's own), and the commands under test start
// processes that carry a run's identity; a run surrounding the test must not be
// that run.
import _ "github.com/codefly-dev/cli/internal/testenv"
