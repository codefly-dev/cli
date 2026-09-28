package processgroup

import (
	"bytes"
	"os"
	"path/filepath"
)

// LaunchWorkspaceEnv carries the workspace whose run started a process. The CLI
// records it in its OWN environment before it starts anything, which is what
// makes every descendant carry it: the agents it spawns, the runners inside
// them, the service binaries those exec, and the stateful stores they bring up.
// Nothing downstream has to cooperate, which matters because agents are
// released independently of this repository and cannot be asked to.
//
// It exists because a working directory cannot answer "whose run is this?". A
// composed module is checked out OUTSIDE the workspace that composes it — a
// pinned module materialises under the module cache, an overlay points at a
// sibling checkout — and its services run with that checkout as their working
// directory, which encloses no workspace or, worse, a different one. Attributing
// by working directory therefore misses precisely the processes a composed run
// adds. The stores are the same gap from the other end: a postgres cluster runs
// from a data directory under ~/.codefly/data, which is in no workspace at all.
const LaunchWorkspaceEnv = "CODEFLY_RUN_WORKSPACE"

// MarkLaunchWorkspace records dir as the workspace this process's run belongs
// to, so everything this process goes on to start is attributable to it.
//
// An empty dir leaves any inherited value in place rather than clearing it: a
// codefly invoked from inside a run, with a working directory in no workspace of
// its own, still belongs to the run that started it.
func MarkLaunchWorkspace(dir string) {
	if dir == "" {
		return
	}
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	_ = os.Setenv(LaunchWorkspaceEnv, filepath.Clean(dir))
}

// LaunchWorkspaceOfProcess is the workspace recorded in pid's environment when
// its run was launched. ok is false when nothing was recorded — a run that was
// already up before this CLI learned to record it, or a process whose
// environment cannot be read — and the caller falls back to the working
// directory.
func LaunchWorkspaceOfProcess(pid int) (string, bool) {
	dir, err := readProcessEnvironmentValue(pid, LaunchWorkspaceEnv)
	if err != nil || dir == "" {
		return "", false
	}
	return dir, true
}

// WorkspaceOfProcess is the workspace a running process is attributed to: the
// run that launched it, and only failing that the workspace enclosing its
// working directory.
//
// The order is the fix. The launching run is the question actually being asked —
// "is this one of mine?" — and the working directory is a guess that is right
// for a service whose code lives inside the workspace and wrong for every
// composed one. Keeping the guess as a fallback is what lets a stop still
// attribute a run that a previous CLI started without recording anything.
func WorkspaceOfProcess(pid int) (string, bool) {
	cwd, err := processWorkingDirectory(pid)
	if err != nil {
		cwd = ""
	}
	return workspaceOfProcess(pid, cwd)
}

// workspaceOfProcess is WorkspaceOfProcess for a caller that has already read
// the process's working directory, so a scan over every process on the machine
// does not read it twice.
func workspaceOfProcess(pid int, cwd string) (string, bool) {
	if workspace, ok := LaunchWorkspaceOfProcess(pid); ok {
		return workspace, true
	}
	if cwd == "" {
		return "", false
	}
	return enclosingWorkspace(cwd)
}

// environmentValue returns the value held for key in a NUL-separated
// environment block, and "" when the block carries no such entry.
func environmentValue(block []byte, key string) string {
	prefix := []byte(key + "=")
	for entry := range bytes.SplitSeq(block, []byte{0}) {
		if value, ok := bytes.CutPrefix(entry, prefix); ok {
			return string(value)
		}
	}
	return ""
}

// readProcessGroupAuthentication returns the process-group authentication pid
// carries, the proof that codefly spawned it.
func readProcessGroupAuthentication(pid int) (string, error) {
	return readProcessEnvironmentValue(pid, groupAuthEnv)
}
