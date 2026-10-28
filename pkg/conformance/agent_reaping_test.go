package conformance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryCommandThatLoadsAgentsReapsThem stops a command from spawning agent
// subprocesses and leaving them running.
//
// Loading a builder or runtime starts an agent process per service. Closing the
// cached connections is what ends them — the agent's own Serve installs a
// SIGTERM handler and shuts down inside the parent's window — so a command that
// loads agents and never calls services.ClearAgents leaks one process per
// service, every invocation.
//
// This is not hypothetical: `generate contracts` loaded builders through
// generators.LoadServiceEndpoints and reaped nothing, while its sibling
// `generate client` reaped correctly. Two regenerations of one workspace left
// eight live private go-grpc agents behind. The omission was invisible because
// nothing failed — the command exited 0 and the processes simply stayed.
//
// A command that deliberately hands its agents to something longer-lived
// (a server, the MCP host) still has to say so by name below, so the exemption
// is a decision on the record rather than an absence.
func TestEveryCommandThatLoadsAgentsReapsThem(t *testing.T) {
	root := repositoryRoot(t)
	// Calls that start an agent subprocess for a service.
	spawns := []string{"LoadBuilder(", "LoadRuntime(", "LoadServiceEndpoints("}
	// Reaping, however the command spells it.
	reaps := []string{"ClearAgents", "ClearAgent(", "RegisterCleanup(services.ClearAgents"}

	// Per COMMAND FILE, which is the granularity that actually holds.
	//
	// Per-file alone is too strict: cmd/sourcefix/fix.go loads while its
	// sibling commands.go defers the reap, and that is correct. Per-package is
	// too loose: cmd/generate hosts many independent commands, so
	// client.go's reap masked contracts.go having none — the very bug this
	// guard exists for passed a per-package check.
	//
	// So the invariant is: a file that declares a command (RunE) AND reaches a
	// spawn must reap in that same file. A helper file with no RunE is cleared,
	// because whichever RunE calls it owns the lifetime.
	var offenders []string
	err := filepath.WalkDir(filepath.Join(root, "cmd"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		body := readFile(t, path)
		if !strings.Contains(body, "RunE:") {
			return nil
		}
		loads := false
		for _, call := range spawns {
			loads = loads || strings.Contains(body, call)
		}
		if !loads {
			return nil
		}
		for _, reap := range reaps {
			if strings.Contains(body, reap) {
				return nil
			}
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		offenders = append(offenders, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(offenders)
	for _, offender := range offenders {
		t.Errorf("%s declares a command that loads service agents but never reaps them: add `defer services.ClearAgents()` beside the command's other defers, or name the file here with the reason its agents outlive it", offender)
	}
}
