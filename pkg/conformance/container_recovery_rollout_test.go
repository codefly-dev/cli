package conformance

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
)

// rolloutAgent is one row of the container-recovery rollout inventory in
// docs/container-recovery-rollout.md.
type rolloutAgent struct {
	identity   string
	kind       string
	repository string
	creates    string
	backends   []string
	rebuild    string
}

var rolloutRow = regexp.MustCompile("(?m)^\\| `(codefly\\.dev/[a-z0-9-]+)` \\| ([a-z]+) \\| `([a-z0-9-]+)` \\| (runtime|companion|runtime\\+companion|none) \\| ([^|]+) \\| (required|not-required) \\|")

// containerRecoveryRollout parses the inventory. A row the pattern does not
// match is not silently dropped: every table line in the document must parse,
// so a typo in a cell fails instead of quietly removing an agent from the gate.
func containerRecoveryRollout(t *testing.T) []rolloutAgent {
	t.Helper()
	document := readFile(t, filepath.Join(repositoryRoot(t), "docs", "container-recovery-rollout.md"))
	var agents []rolloutAgent
	for _, match := range rolloutRow.FindAllStringSubmatch(document, -1) {
		agent := rolloutAgent{
			identity:   match[1],
			kind:       match[2],
			repository: match[3],
			creates:    match[4],
			rebuild:    match[6],
		}
		for _, backend := range strings.Split(match[5], ",") {
			agent.backends = append(agent.backends, strings.TrimSpace(backend))
		}
		agents = append(agents, agent)
	}
	if len(agents) == 0 {
		t.Fatal("docs/container-recovery-rollout.md lists no agents")
	}
	if listed := strings.Count(document, "\n| `codefly.dev/"); listed != len(agents) {
		t.Fatalf("docs/container-recovery-rollout.md has %d agent rows but %d parse; a malformed cell drops an agent from the gate", listed, len(agents))
	}
	return agents
}

// TestRolloutInventoryIsWellFormed rejects an inventory row that names a
// backend or a disposition the rollout has no meaning for, and a repository or
// agent listed twice with different answers.
func TestRolloutInventoryIsWellFormed(t *testing.T) {
	backends := map[string]bool{"local": true, "nix": true, "docker": true, "—": true}
	identities := map[string]bool{}
	repositories := map[string]bool{}
	for _, agent := range containerRecoveryRollout(t) {
		for _, backend := range agent.backends {
			if !backends[backend] {
				t.Errorf("%s declares unknown backend %q", agent.repository, backend)
			}
		}
		identity := agent.kind + " " + agent.identity
		if identities[identity] {
			t.Errorf("%s is listed twice", identity)
		}
		identities[identity] = true
		if repositories[agent.repository] {
			t.Errorf("repository %s is listed twice", agent.repository)
		}
		repositories[agent.repository] = true
	}
}

// TestContainerCreatingAgentsMustBeRebuilt is the invariant the gate exists
// for. The marker's reader is the Core compiled into the agent, so an agent
// that creates containers cannot be carried across the v2 change unrebuilt:
// it would stamp its containers with ownership no sweep can match.
func TestContainerCreatingAgentsMustBeRebuilt(t *testing.T) {
	for _, agent := range containerRecoveryRollout(t) {
		if agent.creates != "none" && agent.rebuild != "required" {
			t.Errorf("%s creates containers (%s) but is marked %s", agent.repository, agent.creates, agent.rebuild)
		}
	}
}

// TestRolloutInventoryCoversEveryPinnedAgent keeps the inventory from going
// stale in the one direction that matters: an agent this repository pins but
// never classified would be published into the fleet without anyone deciding
// whether its Core understands the marker.
func TestRolloutInventoryCoversEveryPinnedAgent(t *testing.T) {
	listed := map[string]bool{}
	for _, agent := range containerRecoveryRollout(t) {
		if agent.kind == "service" {
			listed[agent.identity] = true
		}
	}
	for _, plugin := range sourceworkspace.Roster().Plugins {
		identity := plugin.Publisher + "/" + plugin.Name
		if !listed[identity] {
			t.Errorf("source-workspace roster pins %s, which docs/container-recovery-rollout.md does not classify", identity)
		}
	}
	for _, row := range Default().Rows {
		for _, agent := range row.Agents {
			identity := agent.Publisher + "/" + agent.Name
			if !listed[identity] {
				t.Errorf("conformance row %s drives %s, which docs/container-recovery-rollout.md does not classify", row.ID, identity)
			}
		}
	}
}
