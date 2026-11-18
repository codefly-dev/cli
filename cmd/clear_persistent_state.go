package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/codefly-dev/core/wool"
)

// A persistent run's native state is a developer's database: no run, agent or
// sweep removes it, so every naming scope ever run and stopped leaves a
// directory under the user cache directory for good. `codefly clear` shows
// them and removes the ones a person asks for — by name (--state) or by age
// (--older-than) — and nothing here removes one for any other reason. Whose a
// directory is, whether it is still in use and when a run last claimed it are
// the records its agent left on it (core's runners/runstate).

// persistentState is one persistent run's state directory and what it holds.
type persistentState struct {
	runstate.State
	// size is the bytes of the files under the directory.
	size int64
}

// prunesRunState reports that the person asked for persistent run state to be
// removed, which is then all clear does.
func (options clearOptions) prunesRunState() bool {
	return len(options.states) > 0 || options.olderThan > 0
}

// parseStateAge reads --older-than: a Go duration, or a number of days (30d).
func parseStateAge(value string) (time.Duration, error) {
	var age time.Duration
	var err error
	if days, ok := strings.CutSuffix(value, "d"); ok {
		var n float64
		n, err = strconv.ParseFloat(days, 64)
		age = time.Duration(n * 24 * float64(time.Hour))
	} else {
		age, err = time.ParseDuration(value)
	}
	if err != nil || age <= 0 {
		return 0, fmt.Errorf("--older-than %q is not an age: give a positive duration such as 72h or 30d", value)
	}
	return age, nil
}

// persistentRunState is every persistent run's state directory under root in
// scope, most recently claimed first, in use or not.
func persistentRunState(root string, scope processgroup.Scope) ([]persistentState, error) {
	listed, err := runstate.List(root, func(run runstate.Run) bool {
		return !run.Disposable && scope.Includes(run.Workspace)
	})
	states := make([]persistentState, 0, len(listed))
	for _, state := range listed {
		states = append(states, persistentState{State: state, size: directorySize(state.Dir)})
	}
	sort.SliceStable(states, func(i, j int) bool { return states[i].LastClaimed.After(states[j].LastClaimed) })
	return states, err
}

// directorySize is the bytes of the regular files under dir. What cannot be
// read is not counted: the figure informs a person, it decides nothing.
func directorySize(dir string) int64 {
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})
	return size
}

func humanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func humanAge(age time.Duration) string {
	switch {
	case age >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	case age >= time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
}

func (state persistentState) fields() []*wool.LogField {
	scope := state.Run.NamingScope
	if scope == "" {
		scope = "(none)"
	}
	return []*wool.LogField{
		wool.Field("directory", state.Dir),
		wool.Field("workspace", state.Run.Workspace),
		wool.Field("naming-scope", scope),
		wool.Field("last-claimed", humanAge(time.Since(state.LastClaimed))+" ago"),
		wool.Field("size", humanSize(state.size)),
		wool.Field("in-use", state.InUse),
	}
}

// reportPersistentRunState says what persistent run state the scope keeps: on
// a dry run each directory with its run, age and size, otherwise how many
// there are and how to see and remove them. It removes nothing.
func reportPersistentRunState(w *wool.Wool, root string, options clearOptions) []error {
	states, err := persistentRunState(root, options.scope)
	var total int64
	for _, state := range states {
		total += state.size
		if options.dryRun {
			w.Info("persistent run state (kept)", state.fields()...)
		}
	}
	if len(states) > 0 {
		w.Info("kept persistent run state: a developer's data, removed only when asked",
			wool.Field("count", len(states)), wool.Field("size", humanSize(total)),
			wool.Field("hint", "--dry-run lists it; --state <directory> or --older-than <age> removes it"))
	}
	if err != nil {
		w.Warn("cannot read some native run state", wool.ErrField(err))
		return []error{fmt.Errorf("list persistent run state: %w", err)}
	}
	return nil
}

// pruneRunState removes the persistent run state the person asked for: each
// directory named, and every directory of the scope no run has claimed for
// options.olderThan. A directory in use, one outside the scope and one no run
// is recorded on are refused, each as its own error, and the rest still go.
func pruneRunState(w *wool.Wool, root string, options clearOptions) []error {
	var failures []error
	known, err := runstate.List(root, nil)
	if err != nil {
		w.Warn("cannot read some native run state", wool.ErrField(err))
		failures = append(failures, fmt.Errorf("list run state: %w", err))
	}
	selected := map[string]persistentState{}
	for _, name := range options.states {
		state, err := namedRunState(known, name, options.scope)
		if err != nil {
			w.Warn("run state not removed", wool.ErrField(err))
			failures = append(failures, err)
			continue
		}
		selected[state.Dir] = persistentState{State: state, size: directorySize(state.Dir)}
	}
	if options.olderThan > 0 {
		states, _ := persistentRunState(root, options.scope)
		for _, state := range states {
			if time.Since(state.LastClaimed) < options.olderThan {
				continue
			}
			if state.InUse {
				w.Info("kept persistent run state that is still in use", state.fields()...)
				continue
			}
			selected[state.Dir] = state
		}
	}
	dirs := make([]string, 0, len(selected))
	for dir := range selected {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var removed int
	var freed int64
	for _, dir := range dirs {
		state := selected[dir]
		if options.dryRun {
			w.Info("would remove persistent run state", state.fields()...)
			removed++
			freed += state.size
			continue
		}
		// Core reads the records again as it removes: a run may have claimed
		// the directory since it was listed.
		if err := runstate.Discard(dir); err != nil {
			w.Warn("run state not removed", wool.ErrField(err))
			failures = append(failures, err)
			continue
		}
		w.Info("removed persistent run state", state.fields()...)
		removed++
		freed += state.size
	}
	switch {
	case removed == 0 && len(failures) == 0:
		w.Info("no persistent run state matched")
	case options.dryRun:
		w.Info("persistent run state would be removed", wool.Field("count", removed), wool.Field("size", humanSize(freed)))
	default:
		w.Info("removed persistent run state", wool.Field("count", removed), wool.Field("size", humanSize(freed)))
	}
	return failures
}

// namedRunState is the recorded state directory a person named, when this
// scope may remove it.
func namedRunState(known []runstate.State, name string, scope processgroup.Scope) (runstate.State, error) {
	wanted, err := resolvedPath(name)
	if err != nil {
		return runstate.State{}, fmt.Errorf("%s: %w", name, err)
	}
	for _, state := range known {
		recorded, err := resolvedPath(state.Dir)
		if err != nil || recorded != wanted {
			continue
		}
		switch {
		case !scope.Includes(state.Run.Workspace):
			return state, fmt.Errorf("%s is the state of a run of workspace %s, not of this one: --all reaches it", name, state.Run.Workspace)
		case state.InUse:
			return state, fmt.Errorf("%s: %w: stop the run that uses it first", name, runstate.ErrStateInUse)
		}
		return state, nil
	}
	return runstate.State{}, fmt.Errorf("%s: %w, so nothing says whose it is or whether it is in use, and codefly does not remove it; `codefly clear --dry-run` lists the directories it can", name, runstate.ErrStateNotRecorded)
}

// resolvedPath is path made absolute with its symbolic links followed, so a
// directory is recognised however it was spelled. A path that does not exist
// resolves to its absolute form.
func resolvedPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if errors.Is(err, fs.ErrNotExist) {
		return filepath.Clean(absolute), nil
	}
	if err != nil {
		return "", err
	}
	return resolved, nil
}
