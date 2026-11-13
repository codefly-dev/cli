package orchestration

import (
	"context"
	"errors"
	"fmt"
	"time"

	runners "github.com/codefly-dev/core/runners/base"

	"github.com/codefly-dev/cli/pkg/processgroup"
)

// A run takes back the process groups its own agents registered and left
// running, on every exit: a stop, a failure and a clean finish alike.
//
// An agent's native server (a store's database process, say) is a group of its
// own that the agent registered, so it survives the agent. Normally the agent
// stops it on SIGTERM, but an agent that died first — a crash, a failed start —
// leaves it running with no owner, and the stale sweep that follows the run
// does not always recover it: a group an agent built on an older Core
// registered carries a boot identity the current reader refuses to decide
// about ("incompatible boot identity for unchanged process start"), so the
// sweep keeps it, the next run fails on "address already in use", and the
// operator recovers it by hand with `codefly recover process-group`. That
// command's Core path reads the older boot representation and proves the owner
// absent before it signals, so the run takes the same path, for exactly the
// groups whose recorded owner is one of the agents this run spawned.

// RunReclaim is what ReclaimAfterRun took back.
type RunReclaim struct {
	ProcessGroups []int
	RunRemovals
}

// ReclaimAfterRun takes back what this run's agents started and left behind,
// once they have exited: their orphaned process groups on every exit, then —
// when removeContainers, for a failed run — the containers they created and
// the networks those leave empty. The groups go first: a group still
// registered in the workspace reads as another live run, and while one is
// live no container is removed.
func (flow *Flow) ReclaimAfterRun(ctx context.Context, started time.Time, removeContainers bool) (RunReclaim, error) {
	var reclaim RunReclaim
	recovered, groupsErr := flow.RecoverProcessGroupsOfThisRun(ctx)
	reclaim.ProcessGroups = recovered
	if !removeContainers {
		return reclaim, groupsErr
	}
	removals, containersErr := flow.RemoveContainersCreatedByThisRun(ctx, started)
	reclaim.RunRemovals = removals
	if containersErr != nil {
		containersErr = fmt.Errorf("remove the failed run's containers: %w", containersErr)
	}
	return reclaim, errors.Join(groupsErr, containersErr)
}

// listRegisteredProcessGroups lists the pgids of every live registered group.
// It and the two below are variables so a test can stand in for Core's
// registry.
var listRegisteredProcessGroups = func(ctx context.Context) ([]int, error) {
	groups, err := runners.ListProcessGroups(ctx)
	if err != nil {
		return nil, err
	}
	var pgids []int
	for _, group := range groups {
		if group.Alive() {
			pgids = append(pgids, group.PGID())
		}
	}
	return pgids, nil
}

var previewProcessGroupRecovery = runners.PreviewProcessGroupRecovery

var recoverProcessGroup = runners.RecoverProcessGroup

// RecoverProcessGroupsOfThisRun stops every registered process group whose
// owner is one of this run's agents and is gone, through Core's recovery
// (`codefly recover process-group`), and answers their pgids. Call it once the
// run's agents have exited: Core recovers only a group whose owner is absent,
// so a group a live agent still owns — another run's, or one of this run's
// agents that has not exited yet — is never touched.
func (flow *Flow) RecoverProcessGroupsOfThisRun(ctx context.Context) ([]int, error) {
	if flow == nil || flow.world == nil {
		return nil, nil
	}
	pgids, err := listRegisteredProcessGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registered process groups: %w", err)
	}
	var recovered []int
	var failures []error
	for _, pgid := range pgids {
		// A preview that fails is a group Core cannot recover: its owner is
		// live, or it does not authenticate. Neither is this run's to stop.
		expected, previewErr := previewProcessGroupRecovery(ctx, pgid)
		if previewErr != nil || expected == nil || expected.PGID != pgid || !flow.world.agents.has(expected.Owner.PID) {
			continue
		}
		result, recoverErr := recoverProcessGroup(ctx, *expected)
		if recoverErr == nil && (result == nil || !result.GroupDead || !result.RecordRemoved) {
			recoverErr = errors.New("group death and record removal were not both confirmed")
		}
		if recoverErr != nil {
			failures = append(failures, fmt.Errorf("process group %d, which agent %d of this run left running: %w; recover it with `codefly recover process-group %d --preview`", pgid, expected.Owner.PID, recoverErr, pgid))
			continue
		}
		recovered = append(recovered, pgid)
	}
	return recovered, errors.Join(failures...)
}

// orphanedProcessGroupOf names the registered process group pid belongs to
// when Core can recover it — its owner is gone, so an earlier run left it
// running — and the workspace whose run launched it, when recorded. A
// variable so a test can stand in for the process table and the registry.
var orphanedProcessGroupOf = func(ctx context.Context, pid int) (int, string, bool) {
	identity, err := runners.InspectProcess(pid)
	if err != nil || identity.PGID <= 1 {
		return 0, "", false
	}
	expected, err := runners.PreviewProcessGroupRecovery(ctx, identity.PGID)
	if err != nil || expected == nil {
		return 0, "", false
	}
	workspace, _ := processgroup.LaunchWorkspaceOfProcess(pid)
	return identity.PGID, workspace, true
}

// orphanedGroupHint turns a port holder that is an earlier run's orphan into
// the command that recovers it; empty for any other holder.
func orphanedGroupHint(ctx context.Context, pid int32) string {
	if pid <= 0 {
		return ""
	}
	pgid, workspace, ok := orphanedProcessGroupOf(ctx, int(pid))
	if !ok {
		return ""
	}
	launched := "an earlier codefly run"
	if workspace != "" {
		launched += " of workspace " + workspace
	}
	return fmt.Sprintf(", in process group %d that %s left running after its agent exited: preview it with `codefly recover process-group %d --preview > recovery.json`, then recover it with `codefly recover process-group %d --expect recovery.json`", pgid, launched, pgid, pgid)
}
