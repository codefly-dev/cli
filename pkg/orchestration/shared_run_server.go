package orchestration

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// A store agent may run ONE server for a whole run and attach every one of the
// run's services of that agent to it: the server is then recorded on the run's
// directory by whichever agent started it and read back by every other. Two
// services of one run resolving that agent at two versions are two programs
// with two ideas of that record, sharing one server. service-postgres 0.0.150
// and 0.0.152 record the run's server differently and neither reads the
// other's record, so the second to initialize refuses the server the first
// started ("held by pid N, which this run did not start"), deep in start-up and
// naming a process rather than the two pins that caused it. A run refuses the
// mix before anything is spawned, naming the services, their versions and the
// line that converges them.

// runServerAgent is one service agent that runs a single server per run.
type runServerAgent struct {
	// Agent is the agent's own reference (agent.codefly.yaml: publisher, kind,
	// name), never a service's name.
	Agent resources.Agent
	// Server is what the run's services share, as a refusal names it.
	Server string
	// SharedIn are the runtime contexts in which the agent attaches the run's
	// services to one server. In any other context each service has a server
	// of its own and two versions never meet.
	SharedIn []string
}

// runServerAgents are the agents that run one server per run. The list is what
// the agents do, read from their sources (2026-11-24), and it is short on
// purpose:
//
//   - codefly.dev/postgres: one postmaster per run under the nix runtime
//     context (service-postgres nixpg.go), every service a database on it.
//
// Not listed, because each service keeps a server and a state directory of its
// own: codefly.dev/redis and codefly.dev/clickhouse (state keyed by the
// service's location and naming scope), codefly.dev/object-storage (a
// container per service).
var runServerAgents = []runServerAgent{
	{
		Agent:    resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "postgres"},
		Server:   "Postgres server",
		SharedIn: []string{resources.RuntimeContextNix},
	},
}

// runServerAgentFor is the runServerAgents entry an agent reference answers to.
func runServerAgentFor(agent *resources.Agent) (runServerAgent, bool) {
	if agent == nil {
		return runServerAgent{}, false
	}
	for _, known := range runServerAgents {
		if agent.Publisher == known.Agent.Publisher && agent.Kind == known.Agent.Kind && agent.Name == known.Agent.Name {
			return known, true
		}
	}
	return runServerAgent{}, false
}

// SharedRunServerService is one service attached to a run's shared server, and
// the agent version it resolved to.
type SharedRunServerService struct {
	Unique  string
	Version string
}

// SharedRunServerConflict is one shared server whose services resolve its
// agent at more than one version.
type SharedRunServerConflict struct {
	// Agent is the agent as <publisher>/<name>, the agent-overrides key.
	Agent string
	// Server is what the services share.
	Server string
	// Services are the services sharing it, sorted by unique.
	Services []SharedRunServerService
}

// Newest is the highest version among the services: the one the remediation
// proposes, since an agent is never moved backwards to make a run start.
func (conflict SharedRunServerConflict) Newest() string {
	newest := ""
	for _, service := range conflict.Services {
		if newest == "" || newerVersion(service.Version, newest) {
			newest = service.Version
		}
	}
	return newest
}

func newerVersion(candidate, current string) bool {
	a, errA := semver.NewVersion(candidate)
	b, errB := semver.NewVersion(current)
	if errA != nil || errB != nil {
		return candidate > current
	}
	return a.GreaterThan(b)
}

// Message says which services share which server at which versions.
func (conflict SharedRunServerConflict) Message() string {
	pins := make([]string, 0, len(conflict.Services))
	for _, service := range conflict.Services {
		pins = append(pins, fmt.Sprintf("%s runs %s %s", service.Unique, conflict.Agent, service.Version))
	}
	return fmt.Sprintf("%s, and they share this run's one %s: every version of the agent keeps its own record of that server, so one version refuses the server another started",
		strings.Join(pins, ", "), conflict.Server)
}

// Remediation is the exact change that puts the services on one version.
func (conflict SharedRunServerConflict) Remediation() string {
	newest := conflict.Newest()
	return fmt.Sprintf("run them on one version: add `%s: {%s: %s}` to %s (`codefly update workspace --agent-override %s=%s` writes it), or pin agent version %s in each service's %s",
		resources.AgentOverridesKey, conflict.Agent, newest, resources.WorkspaceConfigurationName,
		conflict.Agent, newest, newest, resources.ServiceConfigurationName)
}

// SharedRunServerError refuses a run whose services would share one server at
// more than one version of its agent.
type SharedRunServerError struct {
	Conflicts []SharedRunServerConflict
}

func (err *SharedRunServerError) Error() string {
	lines := make([]string, 0, len(err.Conflicts))
	for _, conflict := range err.Conflicts {
		lines = append(lines, conflict.Message()+"; "+conflict.Remediation())
	}
	return "cannot start the run: " + strings.Join(lines, "\n")
}

// SharedRunServerConflicts reports every shared server the services would
// attach to at more than one version of its agent. runtimeContext says where a
// service runs; a service outside the contexts its agent shares a server in is
// not attached to one. A nil runtimeContext judges the services as if every one
// of them shared — what a check with no run to ask (the doctor) wants.
func SharedRunServerConflicts(services []*resources.Service, runtimeContext func(*resources.Service) string) []SharedRunServerConflict {
	attached := map[string][]SharedRunServerService{}
	servers := map[string]string{}
	for _, service := range services {
		if service == nil {
			continue
		}
		known, ok := runServerAgentFor(service.Agent)
		if !ok {
			continue
		}
		if runtimeContext != nil && !slices.Contains(known.SharedIn, runtimeContext(service)) {
			continue
		}
		unique := service.Name
		if identity, err := service.Identity(); err == nil {
			unique = identity.Unique()
		}
		key := known.Agent.Publisher + "/" + known.Agent.Name
		servers[key] = known.Server
		attached[key] = append(attached[key], SharedRunServerService{Unique: unique, Version: service.Agent.Version})
	}
	var conflicts []SharedRunServerConflict
	for key, members := range attached {
		versions := map[string]bool{}
		for _, member := range members {
			versions[member.Version] = true
		}
		if len(versions) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].Unique < members[j].Unique })
		conflicts = append(conflicts, SharedRunServerConflict{Agent: key, Server: servers[key], Services: members})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Agent < conflicts[j].Agent })
	return conflicts
}

// checkSharedRunServers refuses the run when services of its run set would
// share one server at more than one version of its agent. uniques is the run
// set; the origin is judged with it unless the run leaves it out, and a
// service bound to a remote environment starts nothing here and is not judged.
//
// It is called twice: before any agent is spawned (admitRunPlan), where a
// service whose context is still "free" is not yet known to share anything,
// and once every "free" service has its concrete backend (resolveBackends).
func (flow *Flow) checkSharedRunServers(uniques []string, remotes map[string]*Remote) error {
	if flow == nil || flow.world == nil || flow.world.Dependencies == nil {
		return nil
	}
	origin := ""
	if flow.originService != nil {
		origin = resources.WithUnique(flow.originService).Unique()
	}
	runSet := append([]string{}, uniques...)
	if origin != "" && !flow.excludeRoot {
		runSet = append(runSet, origin)
	}
	var services []*resources.Service
	contexts := map[*resources.Service]string{}
	seen := map[string]bool{}
	for _, unique := range runSet {
		if seen[unique] || remotes[unique] != nil || (unique == origin && flow.excludeRoot) {
			continue
		}
		seen[unique] = true
		service := flow.originService
		if unique != origin {
			var err error
			if service, err = flow.ServiceFromUnique(unique); err != nil {
				return err
			}
		}
		services = append(services, service)
		contexts[service] = flow.ServiceRuntimeContext(unique)
	}
	conflicts := SharedRunServerConflicts(services, func(service *resources.Service) string { return contexts[service] })
	if len(conflicts) == 0 {
		return nil
	}
	return &SharedRunServerError{Conflicts: conflicts}
}

// admitRunPlan is the last word on a run set before any of its agents is
// spawned: two versions of an agent that runs one server per run would
// otherwise meet on that server at Init, so the run is refused here, and a run
// that stands is described.
func (flow *Flow) admitRunPlan(ctx context.Context, required []string, remotes map[string]*Remote) error {
	if err := flow.checkSharedRunServers(required, remotes); err != nil {
		return err
	}
	if err := flow.logRunPlan(ctx, required, remotes); err != nil {
		return wool.Get(ctx).In("flow.InitManagers").Wrapf(err, "cannot describe service run plan")
	}
	return nil
}

// resolveBackends gives every "free" service of the run its concrete backend
// and judges the run's shared servers again: the agents are loaded, nothing is
// initialized, and a server shared only because the run fell back from Docker
// to nix is known for the first time.
func (flow *Flow) resolveBackends(ctx context.Context, uniques []string, remotes map[string]*Remote) error {
	if err := flow.resolveDockerFallback(ctx); err != nil {
		return wool.Get(ctx).In("flow.resolveBackends").Wrap(err)
	}
	return flow.checkSharedRunServers(uniques, remotes)
}
