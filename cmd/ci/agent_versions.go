package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
)

// Seams for testing the artifact probe without reaching real registries.
var (
	resolveAgentLatest = manager.ResolveLatest
	agentAlreadyLocal  = manager.Downloaded
	githubAssetURL     = manager.DownloadURL
	agentProbeClient   = &http.Client{Timeout: 20 * time.Second}
)

// agentSourceProbe is the outcome of resolving one agent against one source.
type agentSourceProbe struct {
	label        string
	downloadable bool
	detail       string
}

// agentArtifactStatus aggregates every source tried for a single agent pin.
type agentArtifactStatus struct {
	agent   *resources.Agent
	sources []agentSourceProbe
}

func (s agentArtifactStatus) downloadable() bool {
	for _, source := range s.sources {
		if source.downloadable {
			return true
		}
	}
	return false
}

// validateAgentVersions resolves every affected service's pinned agent against
// the configured artifact sources before any CI phase spawns it. A pin that is
// tagged but has no downloadable artifact would otherwise surface as an opaque
// 404 deep inside a phase; here it fails fast with a single, legible report.
//
// Skipped entirely under CODEFLY_AGENT_SOURCE=local, where agents are resolved
// from local builds and no artifact is ever downloaded.
func validateAgentVersions(ctx context.Context, workspace *resources.Workspace, plan *Plan) error {
	if manager.AgentSourceLocal() {
		return nil
	}
	if plan == nil || len(plan.Services) == 0 {
		return nil
	}
	cli.Header(2, "Pre-flight: validating agent versions")

	agents, err := collectPlanAgents(ctx, workspace, plan)
	if err != nil {
		return err
	}

	var unpublished []agentArtifactStatus
	for _, agent := range agents {
		if _, err := resolveAgentLatest(ctx, agent); err != nil {
			unpublished = append(unpublished, agentArtifactStatus{
				agent:   agent,
				sources: []agentSourceProbe{{label: "version resolution", detail: err.Error()}},
			})
			continue
		}
		if local, err := agentAlreadyLocal(ctx, agent); err == nil && local {
			continue
		}
		status := probeAgentArtifact(ctx, agent)
		if !status.downloadable() {
			unpublished = append(unpublished, status)
		}
	}

	if len(unpublished) == 0 {
		return nil
	}
	return errors.New(formatUnpublishedReport(unpublished))
}

// collectPlanAgents returns the distinct agents backing the affected services,
// keyed by kind+identifier so a shared pin is validated once.
func collectPlanAgents(ctx context.Context, workspace *resources.Workspace, plan *Plan) ([]*resources.Agent, error) {
	seen := map[string]bool{}
	var agents []*resources.Agent
	for _, planned := range plan.Services {
		ref, err := resources.ParseServiceWithOptionalModule(planned.Service)
		if err != nil {
			return nil, fmt.Errorf("parse affected service %q: %w", planned.Service, err)
		}
		module, err := workspace.LoadModuleFromName(ctx, ref.Module)
		if err != nil {
			return nil, fmt.Errorf("load module %q: %w", ref.Module, err)
		}
		service, err := module.LoadServiceFromName(ctx, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("load service %q: %w", planned.Service, err)
		}
		if service.Agent == nil {
			continue
		}
		agent := *service.Agent
		if seen[agent.Unique()] {
			continue
		}
		seen[agent.Unique()] = true
		agents = append(agents, &agent)
	}
	return agents, nil
}

// probeAgentArtifact HEADs the GitHub release asset and, when configured, the
// OCI manifest and Nix flake output for the agent.
func probeAgentArtifact(ctx context.Context, agent *resources.Agent) agentArtifactStatus {
	status := agentArtifactStatus{agent: agent}
	status.sources = append(status.sources, probeGitHubAsset(ctx, agent))
	status.sources = append(status.sources, probeOCIManifest(ctx, agent))
	if source, ok := probeNixFlake(ctx, agent); ok {
		status.sources = append(status.sources, source)
	}
	return status
}

func probeGitHubAsset(ctx context.Context, agent *resources.Agent) agentSourceProbe {
	url := githubAssetURL(agent)
	probe := agentSourceProbe{label: "GitHub release asset " + path.Base(url)}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		probe.detail = err.Error()
		return probe
	}
	resp, err := agentProbeClient.Do(req)
	if err != nil {
		probe.detail = err.Error()
		return probe
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		probe.downloadable = true
		probe.detail = "ok"
		return probe
	}
	probe.detail = fmt.Sprintf("%d", resp.StatusCode)
	return probe
}

func probeOCIManifest(ctx context.Context, agent *resources.Agent) agentSourceProbe {
	registry := strings.TrimSpace(os.Getenv("AGENT_REGISTRY"))
	if registry == "" {
		return agentSourceProbe{label: "OCI registry", detail: "not configured (set AGENT_REGISTRY)"}
	}
	reference := fmt.Sprintf("%s/agents/%s/%s:%s", registry, agent.Publisher, agent.Name, agent.Version)
	probe := agentSourceProbe{label: "OCI " + reference}
	store := manager.NewOCIStoreFromEnv(slog.Default())
	if store == nil {
		probe.detail = "not configured (set AGENT_REGISTRY)"
		return probe
	}
	available, err := store.Available(ctx, agent)
	if err != nil {
		probe.detail = err.Error()
		return probe
	}
	if available {
		probe.downloadable = true
		probe.detail = "ok"
		return probe
	}
	probe.detail = "manifest not found"
	return probe
}

func probeNixFlake(ctx context.Context, agent *resources.Agent) (agentSourceProbe, bool) {
	store := manager.NewNixStoreFromEnv(slog.Default())
	if store == nil {
		return agentSourceProbe{}, false
	}
	probe := agentSourceProbe{label: "Nix flake " + os.Getenv("AGENT_NIX_FLAKE")}
	available, err := store.Available(ctx, agent)
	if err != nil {
		probe.detail = err.Error()
		return probe, true
	}
	if available {
		probe.downloadable = true
		probe.detail = "ok"
		return probe, true
	}
	probe.detail = "flake output not found"
	return probe, true
}

func formatUnpublishedReport(statuses []agentArtifactStatus) string {
	var b strings.Builder
	plural := "pin"
	if len(statuses) > 1 {
		plural = "pins"
	}
	fmt.Fprintf(&b, "%d agent %s not downloadable in CI (no published artifact):\n", len(statuses), plural)
	for _, status := range statuses {
		fmt.Fprintf(&b, "\nagent %s is not published (no CI-downloadable artifact)\n", status.agent.Identifier())
		for _, source := range status.sources {
			fmt.Fprintf(&b, "  - %s: %s\n", source.label, source.detail)
		}
		b.WriteString("  -> tag + release the agent, or set AGENT_REGISTRY\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
