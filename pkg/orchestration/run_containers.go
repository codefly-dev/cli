package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/docker/docker/api/types/container"

	"github.com/codefly-dev/cli/pkg/processgroup"
)

// A run that fails takes back the containers its own agents created (#1233).
// Containers outlive a run on purpose: a running one whose owner is gone is
// kept so the next run reuses its dev data (dockerrun.shouldReapContainer).
// A run that failed holds no such data, and its half-started containers hold
// the ports the next run needs.
//
// What a run created is told apart with no new label: each container carries
// the pid of the agent that created it (codefly.session), the run records the
// pid of every agent it spawned, and a container created before the run
// started is never its own, whatever pid it carries.

// ownedLabel is the value Core sets codefly.owner to on every container.
const ownedLabel = "true"

// spawnedAgents is every agent pid one run spawned, recorded as each agent
// loads, so a manager that fails after its agent started still counts.
type spawnedAgents struct {
	mu   sync.Mutex
	pids map[int]bool
}

func (s *spawnedAgents) add(pid int) {
	if pid <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pids == nil {
		s.pids = map[int]bool{}
	}
	s.pids[pid] = true
}

func (s *spawnedAgents) has(pid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pids[pid]
}

// createdByRun reports whether c is a container this run's agents created:
// codefly-owned, labelled with one of the run's agent pids, created no
// earlier than the run started (a recycled pid on an adopted container does
// not count), and not a ledgered data container, which no sweep removes.
func createdByRun(c *container.Summary, agents *spawnedAgents, started time.Time) bool {
	if c.Labels[dockerrun.LabelCodeflyOwner] != ownedLabel || c.Labels[dockerrun.LabelCodeflyInvocation] != "" {
		return false
	}
	pid, err := strconv.Atoi(c.Labels[dockerrun.LabelCodeflySession])
	if err != nil || !agents.has(pid) {
		return false
	}
	return c.Created >= started.Unix()
}

// removeRunContainer force-removes one container. It is a variable so a test
// can stand in for the Docker engine, as listOwnedContainers is.
var removeRunContainer = func(ctx context.Context, id string) error {
	cli, err := dockerrun.NewClient()
	if err != nil {
		return err
	}
	defer cli.Close()
	removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return cli.ContainerRemove(removeCtx, id, container.RemoveOptions{Force: true})
}

// otherRunsLive reports whether another run's managed process groups are
// live in the run's workspace. Nothing records that a run adopted a container
// another created, so while one is live this run removes nothing. A group
// whose ownership cannot be decided is not counted: it is no live run.
var otherRunsLive = func(ctx context.Context, world *World) bool {
	groups, _ := processgroup.PreviewManagedProcessGroups(ctx, processgroup.InWorkspace(world.Workspace.Dir()))
	return len(groups) > 0
}

// RemoveContainersCreatedByThisRun removes the containers this run's agents
// created since started, in the run's recovery scope, once its agents are
// gone; it answers their names. Call it only for a run that failed: a run
// stopped on purpose keeps them for the next run to reuse.
func (flow *Flow) RemoveContainersCreatedByThisRun(ctx context.Context, started time.Time) ([]string, error) {
	scopeID, _, ok := strings.Cut(flow.containerRecoveryIdentity, ":")
	if !ok || scopeID == "" || flow.world == nil || flow.world.Workspace == nil {
		return nil, nil
	}
	if otherRunsLive(ctx, flow.world) {
		return nil, nil
	}
	listed, err := listOwnedContainers(ctx, scopeID)
	if err != nil {
		return nil, fmt.Errorf("list the run's containers: %w", err)
	}
	var removed []string
	var failures []error
	for i := range listed {
		c := &listed[i]
		if !createdByRun(c, &flow.world.agents, started) {
			continue
		}
		name := strings.TrimPrefix(firstName(c), "/")
		if err := removeRunContainer(ctx, c.ID); err != nil {
			failures = append(failures, fmt.Errorf("remove %s: %w", name, err))
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(failures...)
}

func firstName(c *container.Summary) string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return c.ID
}
