package orchestration

import (
	"context"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/codefly-dev/core/runners/runstate"
)

// forcedQuitAgentGrace is how long a forced quit waits for the agents it
// asked to stop before it takes back what the run left: an agent stops what
// it runs on SIGTERM, and what is taken back first is only what no agent
// uses any more.
const forcedQuitAgentGrace = 5 * time.Second

// TakeBackOnForcedQuit is what a forced quit still does for a disposable run,
// in the short time it has (ctx): the person asked to leave now, and the stop
// that was running is abandoned, but nothing runs this run's scope again, so
// nothing else would ever stop its agents or take back what they left.
//
// It asks every agent this run spawned to stop (SIGTERM, which is what the
// ordinary stop sends once the flow is down: the agent stops what it runs and
// exits), waits for them a moment, and then takes back what the run left
// exactly as its ordinary end does (ReclaimAfterRun): the process groups of
// the agents that are gone, the run's native state, its containers with their
// volumes and data, the networks left empty. An agent that is slow to go is
// not waited for: what it still uses is left, to its own watch of this process
// (core's agents.Serve stops an agent whose launcher is gone) and to `codefly
// clear`.
//
// For a run that is not disposable it does nothing: a developer's run is theirs
// to come back to, and a forced quit of it stays what it was.
func (flow *Flow) TakeBackOnForcedQuit(ctx context.Context, started time.Time) (RunReclaim, error) {
	if flow == nil || flow.world == nil || flow.world.Env == nil || flow.world.Workspace == nil {
		return RunReclaim{}, nil
	}
	proto, err := flow.world.Env.Proto()
	if err != nil || !runstate.For(flow.world.Workspace.Dir(), proto).Disposable {
		return RunReclaim{}, nil
	}
	agents := flow.world.agents.own(ctx)
	for _, pid := range agents {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	grace, cancel := context.WithTimeout(ctx, forcedQuitAgentGrace)
	defer cancel()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for len(agents) > 0 {
		agents = slices.DeleteFunc(agents, func(pid int) bool { return !ownChild(grace, pid) })
		if len(agents) == 0 {
			break
		}
		select {
		case <-grace.Done():
			agents = nil
		case <-poll.C:
		}
	}
	return flow.ReclaimAfterRun(ctx, started, false)
}

// own is the agents this run spawned that are still this process's children.
// A recorded pid whose process was reaped may name anything by now.
func (s *spawnedAgents) own(ctx context.Context) []int {
	s.mu.Lock()
	pids := make([]int, 0, len(s.pids))
	for pid := range s.pids {
		pids = append(pids, pid)
	}
	s.mu.Unlock()
	slices.Sort(pids)
	return slices.DeleteFunc(pids, func(pid int) bool { return !ownChild(ctx, pid) })
}

// ownChild reports whether pid is a live child of this process.
func ownChild(ctx context.Context, pid int) bool {
	parent, ok := parentOfProcess(ctx, pid)
	return ok && parent == os.Getpid() && syscall.Kill(pid, 0) == nil
}
