package orchestration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
)

// exitedRun is a run as it stands once its agents have exited: three started
// agents in the hub, two of them (4100, 4200) spawned by this run; Core's
// registry holding the orphaned server group 700 of agent 4100, an orphan of
// another run (800) and a group whose owner is live (900); and the engine
// holding the MinIO and gateway agent 4100 created, on a network of their own.
type exitedRun struct {
	flow      *Flow
	managers  []*recordingManager
	recovered []int
	removed   []string
	networks  []string
}

func newExitedRun(t *testing.T, started time.Time) *exitedRun {
	t.Helper()
	run := &exitedRun{}
	var managers []IManager
	for _, unique := range []string{"saas/frontend", "tracing/evidence", "saas/object-storage"} {
		manager := &recordingManager{unique: unique}
		run.managers = append(run.managers, manager)
		managers = append(managers, manager)
	}
	world := &World{Workspace: &resources.Workspace{}}
	world.agents.add(4100)
	world.agents.add(4200)
	run.flow = &Flow{world: world, hub: &Hub{managers: managers}, containerRecoveryIdentity: "scope-1:namespace-1"}

	previousList, previousPreview, previousRecover := listRegisteredProcessGroups, previewProcessGroupRecovery, recoverProcessGroup
	previousContainers, previousRemove, previousNetwork, previousLive := listOwnedContainers, removeRunContainer, removeRunNetwork, otherRunsLive
	t.Cleanup(func() {
		listRegisteredProcessGroups, previewProcessGroupRecovery, recoverProcessGroup = previousList, previousPreview, previousRecover
		listOwnedContainers, removeRunContainer, removeRunNetwork, otherRunsLive = previousContainers, previousRemove, previousNetwork, previousLive
	})
	listRegisteredProcessGroups = func(context.Context) ([]int, error) { return []int{700, 800, 900}, nil }
	owners := map[int]int{700: 4100, 800: 5100}
	previewProcessGroupRecovery = func(_ context.Context, pgid int) (*runners.ProcessGroupRecoveryExpectation, error) {
		owner, orphaned := owners[pgid]
		if !orphaned {
			return nil, errors.New("original recovery owner is live or has incompatible boot evidence")
		}
		return &runners.ProcessGroupRecoveryExpectation{PGID: pgid, Owner: runners.ProcessIdentity{PID: owner}}, nil
	}
	recoverProcessGroup = func(_ context.Context, expected runners.ProcessGroupRecoveryExpectation) (*runners.ProcessGroupRecoveryResult, error) {
		run.recovered = append(run.recovered, expected.PGID)
		return &runners.ProcessGroupRecoveryResult{GroupDead: true, RecordRemoved: true}, nil
	}

	onNetwork := func(c container.Summary) container.Summary {
		c.NetworkSettings = &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{
			"codefly-saas-object-storage-network": {}, "bridge": {},
		}}
		return c
	}
	listOwnedContainers = func(context.Context, string) ([]container.Summary, error) {
		return []container.Summary{
			onNetwork(summary("minio", 4100, started.Add(time.Second), "")),
			onNetwork(summary("gateway", 4100, started.Add(2*time.Second), "")),
		}, nil
	}
	removeRunContainer = func(_ context.Context, id string) error {
		run.removed = append(run.removed, id)
		return nil
	}
	removeRunNetwork = func(_ context.Context, name string) (bool, error) {
		run.networks = append(run.networks, name)
		return true, nil
	}
	otherRunsLive = func(context.Context, *World) bool { return false }
	return run
}

// A run's exit, as `codefly run` drives it — Stop the flow, then once the
// agents have exited take back what they left — reaches every started agent
// and stops the server group the store agent left behind, whether the run was
// stopped with Ctrl-C or failed to start. Only the failed run removes the
// containers its agents created, and with them the network they shared.
func TestARunsExitTakesBackWhatItsAgentsLeft(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	for name, failed := range map[string]bool{"stopped by SIGINT": false, "failed to start": true} {
		t.Run(name, func(t *testing.T) {
			run := newExitedRun(t, started)

			require.NoError(t, run.flow.Stop())
			for _, manager := range run.managers {
				require.Equal(t, 1, manager.stopCalls, "Stop must reach %s", manager.unique)
			}

			reclaim, err := run.flow.ReclaimAfterRun(context.Background(), started, failed)
			require.NoError(t, err)
			require.Equal(t, []int{700}, reclaim.ProcessGroups, "only the group this run's agent owned")
			require.Equal(t, []int{700}, run.recovered)
			if !failed {
				require.Empty(t, run.removed, "a stopped run keeps its containers for the next run to reuse")
				require.Empty(t, run.networks)
				return
			}
			slices.Sort(run.removed)
			require.Equal(t, []string{"gateway", "minio"}, run.removed)
			require.Equal(t, []string{"codefly-saas-object-storage-network"}, run.networks, "the shared network once, and never the engine's bridge")
			require.Equal(t, []string{"codefly-saas-object-storage-network"}, reclaim.Networks)
		})
	}
}

// A group of this run that Core could not recover is named with the command
// that recovers it by hand, and does not stop the rest.
func TestAnUnrecoveredGroupOfThisRunNamesItsRecovery(t *testing.T) {
	run := newExitedRun(t, time.Unix(1_800_000_000, 0))
	recoverProcessGroup = func(context.Context, runners.ProcessGroupRecoveryExpectation) (*runners.ProcessGroupRecoveryResult, error) {
		return &runners.ProcessGroupRecoveryResult{GroupDead: true}, nil
	}
	recovered, err := run.flow.RecoverProcessGroupsOfThisRun(context.Background())
	require.Empty(t, recovered)
	require.ErrorContains(t, err, "process group 700, which agent 4100 of this run left running")
	require.ErrorContains(t, err, "`codefly recover process-group 700 --preview`")
}

// A port the run needs, held by a group an earlier run left running, is
// reported with that group and the command that recovers it — not only
// "already in use".
func TestAPortHeldByAnEarlierRunsOrphanNamesItsRecovery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	previous := orphanedProcessGroupOf
	t.Cleanup(func() { orphanedProcessGroupOf = previous })
	orphanedProcessGroupOf = func(_ context.Context, pid int) (int, string, bool) {
		return 4242, "/work/platform", pid == os.Getpid()
	}

	runner := &Runner{}
	err = runner.checkInitialPortAvailability(context.Background(), []*basev0.NetworkMapping{nativeMapping("tcp", uint16(held))})
	require.ErrorContains(t, err, fmt.Sprintf("port %d (tcp)", held))
	require.ErrorContains(t, err, "in process group 4242 that an earlier codefly run of workspace /work/platform left running")
	require.ErrorContains(t, err, "`codefly recover process-group 4242 --preview > recovery.json`")

	// Any other holder is reported as before.
	orphanedProcessGroupOf = func(context.Context, int) (int, string, bool) { return 0, "", false }
	err = runner.checkInitialPortAvailability(context.Background(), []*basev0.NetworkMapping{nativeMapping("tcp", uint16(held))})
	require.ErrorContains(t, err, "already in use")
	require.NotContains(t, err.Error(), "codefly recover")
}
