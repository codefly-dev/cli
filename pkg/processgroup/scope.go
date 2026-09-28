package processgroup

import (
	"os"
	"path/filepath"
)

// Scope bounds which process groups a reap is allowed to touch.
//
// It exists because this package's reapers are machine-wide by construction:
// the registry is one flat directory shared by every workspace on the machine,
// and the orphan scanners enumerate every process. That is right for garbage
// collecting records whose owner is gone, and wrong for stopping live groups —
// a stop issued in one workspace would signal another workspace's running
// services, which on a machine with several checkouts is a routine accident
// rather than an edge case.
//
// A zero Scope means every workspace, so a caller that has not thought about
// scope keeps the old machine-wide behaviour and has to ask for it by name.
type Scope struct {
	// Workspace is the absolute workspace root a group must belong to. Empty
	// means every workspace.
	Workspace string
}

// AllWorkspaces is the machine-wide scope: every group, whichever workspace it
// belongs to, including groups no workspace can be found for.
func AllWorkspaces() Scope { return Scope{} }

// InWorkspace bounds a reap to the workspace rooted at dir. A group is in scope
// only when its own workspace resolves to the same root, so a group whose
// workspace cannot be determined is out of scope and left running.
func InWorkspace(dir string) Scope {
	if dir == "" {
		return Scope{}
	}
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	return Scope{Workspace: filepath.Clean(dir)}
}

// CurrentWorkspace is the workspace enclosing dir, walking up to its root. The
// second result is false when dir sits in no workspace, which a caller must
// treat as "do not scope by workspace" rather than as an empty scope: an empty
// Scope means machine-wide, which is the opposite.
func CurrentWorkspace(dir string) (string, bool) { return enclosingWorkspace(dir) }

// All reports whether the scope admits every workspace.
func (s Scope) All() bool { return s.Workspace == "" }

// Includes reports whether a group belonging to workspace is in scope. An
// unresolvable workspace ("") is admitted only by the machine-wide scope: when
// a stop is scoped, a group it cannot attribute is one it must not signal.
func (s Scope) Includes(workspace string) bool {
	if s.All() {
		return true
	}
	if workspace == "" {
		return false
	}
	if absolute, err := filepath.Abs(workspace); err == nil {
		workspace = absolute
	}
	return sameDirectory(filepath.Clean(workspace), s.Workspace)
}

// includesProcessGroup resolves a group's workspace from its leader and reports
// whether it is in scope.
//
// A registry record carries no workspace of its own — independently released
// agents write these and this package cannot change their format — so the
// leader is asked instead: the run recorded in its environment when it was
// launched, and only failing that the workspace enclosing its working
// directory. Asking the working directory first was the whole defect: a
// composed module's service runs from a checkout outside the workspace that
// composed it, so a scoped stop left it running and reported success.
func (s Scope) includesProcessGroup(pgid int) bool {
	if s.All() {
		return true
	}
	workspace, ok := WorkspaceOfProcess(pgid)
	if !ok {
		return false
	}
	return s.Includes(workspace)
}

// sameDirectory compares two directory paths, preferring identity on disk so a
// symlinked or /private-prefixed checkout is not mistaken for another
// workspace. It falls back to string equality when either path cannot be
// stat'ed, which is the case for a workspace whose process has just exited.
func sameDirectory(first, second string) bool {
	if first == second {
		return true
	}
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	return os.SameFile(firstInfo, secondInfo)
}
