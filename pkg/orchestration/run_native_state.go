package orchestration

import (
	"fmt"

	"github.com/codefly-dev/core/runners/runstate"
)

// nativeStateRoot is the machine directory agents keep native state under. A
// variable so a test can stand in for the user's cache directory.
var nativeStateRoot = runstate.Root

// RemoveNativeStateOfThisRun removes the native state directories this run's
// store agents left (a database run as a host process keeps its cluster under
// the user cache directory), and answers them. Call it once the run's agents
// have exited and what they left running is stopped.
//
// Only a disposable run has any: one whose naming scope this invocation
// generated (IsolateEnvironment), which nothing can run again. A stopped run
// stops its agents and never destroys them, and an agent keeps its data across
// a stop because a service restarts inside a run; so the invocation that
// declared the state throwaway is the one that takes it back. A plain run, or a
// scope the caller named, is a developer's database and is never removed.
//
// Which directory belongs to which run, and whether anything still uses it, is
// recorded on the directory by the agent that owns it (core's
// runners/runstate): a directory still in use is left for `codefly clear`.
//
// A server left running in one of this run's directories by agents whose own
// launcher is gone is stopped first, as core's runstate.StopAbandoned decides
// it: only the processes those agents recorded there, each checked against its
// recorded birth. What this run's own agents started is not that: this run
// still lives, and it stops them through their process groups
// (RecoverProcessGroupsOfThisRun).
func (flow *Flow) RemoveNativeStateOfThisRun() ([]string, error) {
	if flow == nil || flow.world == nil || flow.world.Env == nil || flow.world.Workspace == nil {
		return nil, nil
	}
	env := flow.world.Env
	if !env.Disposable || env.NamingScope == "" {
		return nil, nil
	}
	root, err := nativeStateRoot()
	if err != nil {
		return nil, fmt.Errorf("locate this run's native state: %w", err)
	}
	workspace, scope := flow.world.Workspace.Dir(), env.NamingScope
	swept, err := runstate.Sweep(root, runstate.SweepOptions{
		Include: func(run runstate.Run) bool {
			return run.Workspace == workspace && run.NamingScope == scope
		},
		StopAbandoned: &runstate.StopOptions{},
	})
	directories := make([]string, 0, len(swept))
	for _, state := range swept {
		directories = append(directories, state.Dir)
	}
	if err != nil {
		return directories, fmt.Errorf("remove this run's native state: %w", err)
	}
	return directories, nil
}
