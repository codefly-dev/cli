package orchestration

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/docker/docker/api/types/container"
)

// summary is a container as the Docker engine lists it: created at created,
// labelled by the agent with pid, and ledgered when invocation is set.
func summary(id string, pid int, created time.Time, invocation string) container.Summary {
	labels := map[string]string{
		dockerrun.LabelCodeflyOwner:   "true",
		dockerrun.LabelCodeflySession: strconv.Itoa(pid),
	}
	if invocation != "" {
		labels[dockerrun.LabelCodeflyInvocation] = invocation
	}
	return container.Summary{ID: id, Names: []string{"/" + id}, Created: created.Unix(), Labels: labels}
}

func TestOnlyContainersThisRunsAgentsCreatedAreItsOwn(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	var agents spawnedAgents
	agents.add(4100)
	agents.add(4200)
	for name, tc := range map[string]struct {
		c    container.Summary
		want bool
	}{
		"created by one of this run's agents":     {summary("ours", 4100, started.Add(time.Second), ""), true},
		"created in the run's first second":       {summary("ours-early", 4200, started, ""), true},
		"by another agent":                        {summary("theirs", 5100, started.Add(time.Second), ""), false},
		"before the run started (a recycled pid)": {summary("adopted", 4100, started.Add(-time.Hour), ""), false},
		"a ledgered (data) container of this run": {summary("ledger", 4100, started.Add(time.Second), "inv-1"), false},
		"with no owner label":                     {container.Summary{ID: "bare", Created: started.Add(time.Second).Unix(), Labels: map[string]string{dockerrun.LabelCodeflySession: "4100"}}, false},
		"with a session label that is not a pid":  {summary("odd", 0, started.Add(time.Second), ""), false},
	} {
		if tc.c.ID == "odd" {
			tc.c.Labels[dockerrun.LabelCodeflySession] = "cli"
		}
		if got := createdByRun(&tc.c, &agents, started); got != tc.want {
			t.Errorf("%s: createdByRun = %v, want %v", name, got, tc.want)
		}
	}
}

// failedRunFlow is a flow whose run started at started, spawned agents pids,
// and resolved one recovery scope; the engine and the process-group view are
// stood in for, and every removal is recorded.
func failedRunFlow(t *testing.T, started time.Time, listed []container.Summary, otherRunLive bool, pids ...int) (*Flow, *[]string) {
	t.Helper()
	world := &World{Workspace: &resources.Workspace{}}
	for _, pid := range pids {
		world.agents.add(pid)
	}
	flow := &Flow{world: world, containerRecoveryIdentity: "scope-1:namespace-1"}
	previousList, previousRemove, previousLive := listOwnedContainers, removeRunContainer, otherRunsLive
	t.Cleanup(func() {
		listOwnedContainers, removeRunContainer, otherRunsLive = previousList, previousRemove, previousLive
	})
	listOwnedContainers = func(_ context.Context, scopeID string) ([]container.Summary, error) {
		if scopeID != "scope-1" {
			t.Errorf("listed scope %q, want the run's", scopeID)
		}
		return listed, nil
	}
	removed := &[]string{}
	removeRunContainer = func(_ context.Context, id string) error {
		*removed = append(*removed, id)
		return nil
	}
	otherRunsLive = func(context.Context, *World) bool { return otherRunLive }
	return flow, removed
}

// A run that failed takes back the containers its own agents created, and
// only those: what it adopted, what another run made and its data are kept.
func TestAFailedRunRemovesOnlyTheContainersItCreated(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	listed := []container.Summary{
		summary("minio", 4100, started.Add(2*time.Second), ""),
		summary("gateway", 4100, started.Add(3*time.Second), ""),
		summary("adopted", 4100, started.Add(-time.Hour), ""),
		summary("theirs", 5100, started.Add(time.Second), ""),
		summary("ledger", 4200, started.Add(time.Second), "inv-1"),
	}
	flow, removed := failedRunFlow(t, started, listed, false, 4100, 4200)
	got, err := flow.RemoveContainersCreatedByThisRun(context.Background(), started)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(*removed)
	if !slices.Equal(*removed, []string{"gateway", "minio"}) || len(got) != 2 {
		t.Fatalf("removed %v (reported %v), want only the run's own minio and gateway", *removed, got)
	}
}

// While another run is live in the workspace it may have adopted what this
// one created; nothing records that, so nothing is removed.
func TestAFailedRunKeepsItsContainersWhileAnotherRunIsLive(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	flow, removed := failedRunFlow(t, started, []container.Summary{summary("minio", 4100, started.Add(time.Second), "")}, true, 4100)
	got, err := flow.RemoveContainersCreatedByThisRun(context.Background(), started)
	if err != nil || len(*removed) != 0 || len(got) != 0 {
		t.Fatalf("removed %v (reported %v, err %v), want nothing while another run is live", *removed, got, err)
	}
}
