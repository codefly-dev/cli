// Package testenv makes a test binary deaf to the Codefly run it was started
// under.
//
// A `codefly run` hands every process it starts the run's own identity: the
// service coordinates and addresses under CODEFLY__, the agent bearer, the
// container-recovery marker, the process-group authentication and the launching
// workspace. A shell opened from such a process inherits all of it, and so does
// a `go test` typed into that shell. The tests then read another run's identity
// as their own: a container is refused because the recovery marker names a
// process that is not the test's parent, a scan attributes the test's children
// to the surrounding workspace.
//
// Import it for its side effect from one test file of a package whose tests
// read, or start processes that read, any of those variables:
//
//	import _ "github.com/codefly-dev/cli/internal/testenv"
//
// It depends on syscall alone, deliberately. Go initializes the packages of a
// binary in import-path order among those whose dependencies are done, so a
// package that needs only syscall is initialized before any package of ours
// that reads the environment while initializing (a flag default, a captured
// variable).
//
// What a test sets on purpose is kept, in the test and in what it starts: the
// first binary to clear marks the environment, and a child that finds the mark
// (a test binary re-executed as a helper process) clears nothing.
package testenv

import "syscall"

// Cleared marks an environment this package has already cleared. Everything
// present beside it was put there by a test.
const Cleared = "CODEFLY_TEST_ENVIRONMENT_CLEARED"

// InjectedPrefix is the prefix of everything a run injects into a service
// process: coordinates, endpoints, configurations, the runtime context.
const InjectedPrefix = "CODEFLY__"

// RunMarkers are the variables a run hands the processes it starts to say whose
// they are. Each is named after the constant that owns it where one is
// exported; the test of this package holds the two in step.
var RunMarkers = []string{
	"CODEFLY_AGENT_TOKEN",
	"CODEFLY_CONTAINER_RECOVERY_SCOPE",
	"CODEFLY_PROCESS_GROUP_AUTH",
	"CODEFLY_RUN_WORKSPACE",
}

func init() {
	if _, marked := syscall.Getenv(Cleared); marked {
		return
	}
	for _, name := range Inherited(syscall.Environ()) {
		_ = syscall.Unsetenv(name)
	}
	_ = syscall.Setenv(Cleared, "1")
}

// Inherited names the variables of environ, given as NAME=value, that belong to
// a surrounding run.
func Inherited(environ []string) []string {
	var names []string
	for _, entry := range environ {
		name := entry
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				name = entry[:i]
				break
			}
		}
		if belongsToRun(name) {
			names = append(names, name)
		}
	}
	return names
}

func belongsToRun(name string) bool {
	if len(name) >= len(InjectedPrefix) && name[:len(InjectedPrefix)] == InjectedPrefix {
		return true
	}
	for _, marker := range RunMarkers {
		if name == marker {
			return true
		}
	}
	return false
}
