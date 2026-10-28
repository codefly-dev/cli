package conformance

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// codeOf returns the file's source with every comment removed, by parsing it
// and printing the syntax tree back out. The checks below are textual, and a
// comment is text: the first version of this guard matched the word
// "ClearAgents" anywhere in the file, so the explanatory comment that sits
// above the real `defer services.ClearAgents()` was itself enough to satisfy
// it — deleting the defer and keeping the comment passed. Stripping comments
// first is what makes a reap have to be a reap.
func codeOf(t *testing.T, path, body string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, body, 0)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var code bytes.Buffer
	if err := printer.Fprint(&code, fset, file); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return code.String()
}

// TestNoCommandFileOmitsAgentReaping is a TEXTUAL omission detector, and
// nothing more: it reads source as text — comments stripped — and asks whether
// a reaping call appears. It catches the omission that actually happened: a
// command that loads agents with no reap anywhere in its file. It is a cheap
// backstop against the same slip recurring, not proof that any command reaps
// correctly, and it is deliberately not a call-graph analysis: a reap written
// behind a condition that never runs would still satisfy it.
//
// Loading a service starts an agent process. ClearAgents closes the cached
// connections, and Core's close is cooperative in itself — it asks the agent
// to stop, waits for the agent to reap its own children, and forces the
// process group down only if that wait times out. (The signal is Core's
// choice; the comments around it in Core do not match the code it runs, so
// this does not restate a mechanism.) A command that loads agents and never
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
		body := codeOf(t, path, readFile(t, path))
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
