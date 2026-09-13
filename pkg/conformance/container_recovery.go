package conformance

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// ContainerRecoveryRolloutRelativePath locates the rollout inventory from the
// repository root.
const ContainerRecoveryRolloutRelativePath = "pkg/conformance/container_recovery.json"

//go:embed container_recovery.json
var embeddedContainerRecoveryRollout []byte

// ContainerCreation is how an agent reaches a recovery-labeled container.
type ContainerCreation string

const (
	// CreatesInRuntime means the agent's own code builds a dockerrun environment.
	CreatesInRuntime ContainerCreation = "runtime"
	// CreatesInCompanion means it reaches one only through a Core package.
	CreatesInCompanion ContainerCreation = "companion"
	// CreatesInBoth means it does both.
	CreatesInBoth ContainerCreation = "runtime+companion"
	// CreatesNothing means no path in the binary creates a container.
	CreatesNothing ContainerCreation = "none"
)

// Rebuild is whether an agent must be rebuilt on the marker's Core before the
// fleet release.
type Rebuild string

const (
	// RebuildRequired means the agent cannot ship against the new CLI unrebuilt.
	RebuildRequired Rebuild = "required"
	// RebuildNotRequired means the marker never reaches a consumer inside it.
	RebuildNotRequired Rebuild = "not-required"
)

// RolloutAgent is one agent repository's position in the rollout.
type RolloutAgent struct {
	Repository string            `json:"repository"`
	Kind       string            `json:"kind"`
	Publisher  string            `json:"publisher"`
	Name       string            `json:"name"`
	Creates    ContainerCreation `json:"creates"`
	Backends   []string          `json:"backends,omitempty"`
	Rebuild    Rebuild           `json:"rebuild"`
}

// Identity renders the agent the way the roster and the matrix spell it.
func (a *RolloutAgent) Identity() string { return a.Publisher + "/" + a.Name }

// ContainerRecoveryRollout is the whole inventory.
type ContainerRecoveryRollout struct {
	SchemaVersion int    `json:"schema_version"`
	Core          string `json:"core"`
	Snapshot      string `json:"snapshot"`
	// MarkerProjectedBy lists the packages that project the marker into the
	// agents they spawn. It is not a detail of how the CLI is organized: an
	// agent only reads a marker on the paths something wrote one, so a command
	// absent here creates its containers unlabeled however new the agent is,
	// and rebuilding the fleet does not change that.
	MarkerProjectedBy []string       `json:"marker_projected_by"`
	Agents            []RolloutAgent `json:"agents"`
}

var defaultContainerRecoveryRollout = mustParseRollout(embeddedContainerRecoveryRollout)

// Rollout returns the inventory compiled into this build.
func Rollout() ContainerRecoveryRollout { return defaultContainerRecoveryRollout }

func mustParseRollout(payload []byte) ContainerRecoveryRollout {
	rollout, err := ParseContainerRecoveryRollout(payload)
	if err != nil {
		panic(err)
	}
	return rollout
}

// ParseContainerRecoveryRollout decodes and validates an inventory document.
func ParseContainerRecoveryRollout(payload []byte) (ContainerRecoveryRollout, error) {
	var rollout ContainerRecoveryRollout
	if err := json.Unmarshal(payload, &rollout); err != nil {
		return ContainerRecoveryRollout{}, fmt.Errorf("parse container recovery rollout: %w", err)
	}
	if err := rollout.Validate(); err != nil {
		return ContainerRecoveryRollout{}, err
	}
	return rollout, nil
}

// ByKindAndIdentity resolves a pin that knows which kind of agent it names.
func (r *ContainerRecoveryRollout) ByKindAndIdentity(kind, identity string) (RolloutAgent, error) {
	for i := range r.Agents {
		agent := &r.Agents[i]
		if agent.Kind == kind && agent.Identity() == identity {
			return *agent, nil
		}
	}
	return RolloutAgent{}, fmt.Errorf("%s %s is not classified in %s", kind, identity, ContainerRecoveryRolloutRelativePath)
}

// ByIdentity resolves a publisher/name that carries no agent kind — which is
// how the conformance matrix spells an agent pin. An identity two kinds share
// cannot be resolved that way, and returning either one would answer a question
// about a different binary, so it is reported as ambiguous instead.
func (r *ContainerRecoveryRollout) ByIdentity(identity string) (RolloutAgent, error) {
	var found []RolloutAgent
	for i := range r.Agents {
		if r.Agents[i].Identity() == identity {
			found = append(found, r.Agents[i])
		}
	}
	switch len(found) {
	case 0:
		return RolloutAgent{}, fmt.Errorf("%s is not classified in %s", identity, ContainerRecoveryRolloutRelativePath)
	case 1:
		return found[0], nil
	default:
		kinds := make([]string, 0, len(found))
		for _, agent := range found {
			kinds = append(kinds, agent.Kind)
		}
		return RolloutAgent{}, fmt.Errorf("%s is ambiguous across kinds %v; the pin must name the kind to say which binary it means", identity, kinds)
	}
}

// Validate rejects an inventory that cannot be acted on.
func (r *ContainerRecoveryRollout) Validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("container recovery rollout schema_version = %d, want 1", r.SchemaVersion)
	}
	if len(r.Agents) == 0 {
		return fmt.Errorf("container recovery rollout lists no agents")
	}
	if len(r.MarkerProjectedBy) == 0 {
		return fmt.Errorf("container recovery rollout names no marker projection site")
	}
	backends := map[string]bool{"local": true, "nix": true, "docker": true}
	creations := map[ContainerCreation]bool{
		CreatesInRuntime: true, CreatesInCompanion: true, CreatesInBoth: true, CreatesNothing: true,
	}
	rebuilds := map[Rebuild]bool{RebuildRequired: true, RebuildNotRequired: true}
	repositories := map[string]bool{}
	identities := map[string]bool{}
	for i := range r.Agents {
		agent := &r.Agents[i]
		if agent.Repository == "" || agent.Kind == "" || agent.Publisher == "" || agent.Name == "" {
			return fmt.Errorf("container recovery rollout agent %d must have repository, kind, publisher and name", i)
		}
		if !creations[agent.Creates] {
			return fmt.Errorf("%s declares unknown container creation %q", agent.Repository, agent.Creates)
		}
		if !rebuilds[agent.Rebuild] {
			return fmt.Errorf("%s declares unknown rebuild disposition %q", agent.Repository, agent.Rebuild)
		}
		// The marker's reader is the Core compiled into the agent, so an agent
		// that creates containers cannot be carried across the v2 change
		// unrebuilt: it would stamp its containers with ownership no sweep can
		// ever match.
		if agent.Creates != CreatesNothing && agent.Rebuild != RebuildRequired {
			return fmt.Errorf("%s creates containers (%s) but is marked %s", agent.Repository, agent.Creates, agent.Rebuild)
		}
		for _, backend := range agent.Backends {
			if !backends[backend] {
				return fmt.Errorf("%s declares unknown backend %q", agent.Repository, backend)
			}
		}
		if repositories[agent.Repository] {
			return fmt.Errorf("container recovery rollout repeats repository %s", agent.Repository)
		}
		repositories[agent.Repository] = true
		identity := agent.Kind + " " + agent.Identity()
		if identities[identity] {
			return fmt.Errorf("container recovery rollout repeats agent %s", identity)
		}
		identities[identity] = true
	}
	return nil
}
