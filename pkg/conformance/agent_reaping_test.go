package conformance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoCommandFileOmitsAgentReaping is a TEXTUAL omission detector, and
// nothing more: it reads source as text and asks whether a reaping call
// appears. It catches the omission that actually happened — a command that
// loads agents with no reap anywhere in its file — and it is defeated by
// anything that merely mentions the call, a `// TODO: ClearAgents` comment
// included. It is a cheap backstop against the same slip recurring, not proof
// that any command reaps correctly, and it is deliberately not a call-graph
// analysis.
//
// Loading a service starts an agent process. ClearAgents closes the cached
// connections and Core's close is graceful in itself (services/agent.go:
// SIGTERM, wait, SIGKILL fallback), so a command that loads agents and never
// reaps leaks one process per service, every invocation. It retires the
// processes that command started; agents owned by another command are not
// affected.
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
func TestNoCommandFileOmitsAgentReaping(t *testing.T) {
	root := repositoryRoot(t)
	// Calls that start an agent subprocess for a service.
	// services.Load is where the agent process is actually started;
	// LoadBuilder/LoadRuntime then attach to it. All of them are listed because
	// a command reaching any of them has an agent to answer for.
	spawns := []string{"services.Load(", "LoadBuilder(", "LoadRuntime(", "LoadServiceEndpoints("}
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
