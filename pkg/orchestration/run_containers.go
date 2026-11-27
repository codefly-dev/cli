package orchestration

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

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
	_, err = cli.ContainerRemove(removeCtx, id, client.ContainerRemoveOptions{Force: true})
	return err
}

// otherRunsLive reports whether another run's managed process groups are
// live in the run's workspace. Nothing records that a run adopted a container
// another created, so while one is live this run removes nothing. A group
// whose ownership cannot be decided is not counted: it is no live run.
var otherRunsLive = func(ctx context.Context, world *World) bool {
	groups, _ := processgroup.PreviewManagedProcessGroups(ctx, processgroup.InWorkspace(world.Workspace.Dir()))
	return len(groups) > 0
}

// removeRunNetwork removes one network a removed container was attached to,
// when codefly created it and nothing is attached to it any more; it reports
// whether it did. An agent that gives its containers a network of their own
// (the object store's MinIO and gateway share one) removes it only in Destroy,
// which a run never calls, so each failed run used to leave one behind until
// the engine ran out of address pools. A network another container still uses
// is kept for it, stopped containers included: the engine's inspect lists only
// running endpoints, and a stopped container whose network is removed can never
// start again. A variable so a test can stand in for the engine.
var removeRunNetwork = func(ctx context.Context, name string) (bool, error) {
	return inspectUnusedNetwork(ctx, name, false, true)
}

// RecoverUnusedNetwork previews or removes a single explicitly identified,
// Codefly-owned network that no container, including a stopped one, uses.
func RecoverUnusedNetwork(ctx context.Context, id string, remove bool) (bool, error) {
	if len(id) != 64 || id != strings.ToLower(id) {
		return false, errors.New("expected a full lowercase 64-hex Docker network ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return false, errors.New("expected a full lowercase 64-hex Docker network ID")
	}
	return inspectUnusedNetwork(ctx, id, true, remove)
}

func inspectUnusedNetwork(ctx context.Context, ref string, exactID, remove bool) (bool, error) {
	cli, err := dockerrun.NewClient()
	if err != nil {
		return false, err
	}
	defer cli.Close()
	removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inspected, err := cli.NetworkInspect(removeCtx, ref, client.NetworkInspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	inspect := inspected.Network
	if exactID && (inspect.ID != ref || inspect.Name == "" || inspect.Labels[dockerrun.LabelCodeflyName] != inspect.Name) {
		return false, nil
	}
	if inspect.Labels[dockerrun.LabelCodeflyOwner] != ownedLabel || len(inspect.Containers) > 0 {
		return false, nil
	}
	// By name as well as id (the engine ORs them): a container created but
	// never started records its network by name alone.
	attached, err := cli.ContainerList(removeCtx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("network", inspect.ID).Add("network", inspect.Name)})
	if err != nil {
		return false, err
	}
	if len(attached.Items) > 0 {
		return false, nil
	}
	if !remove {
		return true, nil
	}
	if _, err = cli.NetworkRemove(removeCtx, inspect.ID, client.NetworkRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// runNetworks is every network a container of this run was attached to, noted
// while the container still existed (noteRunNetworks).
type runNetworks struct {
	mu    sync.Mutex
	names map[string]bool
}

func (n *runNetworks) add(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.names == nil {
		n.names = map[string]bool{}
	}
	n.names[name] = true
}

func (n *runNetworks) sorted() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	names := make([]string, 0, len(n.names))
	for name := range n.names {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ReachesDocker reports whether a service in runtimeContext can reach the
// Docker control plane: every context but an explicit Local or Nix one.
func ReachesDocker(runtimeContext string) bool {
	return runtimeContext != resources.RuntimeContextNative && runtimeContext != resources.RuntimeContextNix
}

// noteRunNetworks records the networks the containers of this run's agents are
// attached to. Stop and Shutdown call it before they reach an agent: an agent
// that removes its containers there (a disposable run's, or any on Destroy)
// may leave their network, and a removed container no longer names it. A
// container's age is not asked here, unlike createdByRun: a noted network is
// only ever removed once nothing is attached to it.
func (flow *Flow) noteRunNetworks(ctx context.Context) {
	if flow == nil || flow.world == nil || !slices.ContainsFunc(flow.SelectableRuntimeContexts(), ReachesDocker) {
		return
	}
	scopeID, _, ok := strings.Cut(flow.containerRecoveryIdentity, ":")
	if !ok || scopeID == "" {
		return
	}
	listed, err := listOwnedContainers(ctx, scopeID)
	if err != nil {
		return
	}
	for i := range listed {
		if !createdByRun(&listed[i], &flow.world.agents, time.Time{}) {
			continue
		}
		for _, name := range userNetworks(&listed[i]) {
			flow.world.networks.add(name)
		}
	}
}

// RemoveNetworksThisRunLeftEmpty removes the networks noted for this run that
// codefly created and nothing is attached to any more, once its agents are
// gone, whichever way the run ended. A run stopped on purpose that keeps its
// containers keeps their network with them; one whose containers are gone,
// removed by its agents on Stop or Destroy, takes the network back.
func (flow *Flow) RemoveNetworksThisRunLeftEmpty(ctx context.Context) ([]string, error) {
	if flow == nil || flow.world == nil || flow.world.Workspace == nil {
		return nil, nil
	}
	noted := flow.world.networks.sorted()
	if len(noted) == 0 || otherRunsLive(ctx, flow.world) {
		return nil, nil
	}
	var removed []string
	var failures []error
	for _, name := range noted {
		gone, err := removeRunNetwork(ctx, name)
		if err != nil {
			failures = append(failures, fmt.Errorf("remove network %s: %w", name, err))
			continue
		}
		if gone {
			removed = append(removed, name)
		}
	}
	return removed, errors.Join(failures...)
}

// RunRemovals names what RemoveContainersCreatedByThisRun took back.
type RunRemovals struct {
	Containers []string
	Networks   []string
}

// RemoveContainersCreatedByThisRun removes the containers this run's agents
// created since started, in the run's recovery scope, once its agents are
// gone, and then the codefly networks they leave empty. Call it only for a run
// that failed: a run stopped on purpose keeps them for the next run to reuse.
func (flow *Flow) RemoveContainersCreatedByThisRun(ctx context.Context, started time.Time) (RunRemovals, error) {
	var removals RunRemovals
	if flow == nil {
		return removals, nil
	}
	scopeID, _, ok := strings.Cut(flow.containerRecoveryIdentity, ":")
	if !ok || scopeID == "" || flow.world == nil || flow.world.Workspace == nil {
		return removals, nil
	}
	if otherRunsLive(ctx, flow.world) {
		return removals, nil
	}
	listed, err := listOwnedContainers(ctx, scopeID)
	if err != nil {
		return removals, fmt.Errorf("list the run's containers: %w", err)
	}
	var failures []error
	var networks []string
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
		removals.Containers = append(removals.Containers, name)
		for _, attached := range userNetworks(c) {
			if !slices.Contains(networks, attached) {
				networks = append(networks, attached)
			}
		}
	}
	slices.Sort(networks)
	for _, name := range networks {
		removed, err := removeRunNetwork(ctx, name)
		if err != nil {
			failures = append(failures, fmt.Errorf("remove network %s: %w", name, err))
			continue
		}
		if removed {
			removals.Networks = append(removals.Networks, name)
		}
	}
	return removals, errors.Join(failures...)
}

// userNetworks is every network c is attached to other than the engine's own.
func userNetworks(c *container.Summary) []string {
	if c.NetworkSettings == nil {
		return nil
	}
	var names []string
	for name := range c.NetworkSettings.Networks {
		switch name {
		case network.NetworkBridge, network.NetworkHost, network.NetworkNone, "":
			continue
		}
		names = append(names, name)
	}
	return names
}

func firstName(c *container.Summary) string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return c.ID
}
