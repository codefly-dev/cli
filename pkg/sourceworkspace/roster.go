package sourceworkspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	codecore "github.com/codefly-dev/core/code"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// Plugin is information obtained from an installed agent process, not a
// compiled catalog or an assertion that an artifact version is compatible.
type Plugin struct {
	Agent *resources.Agent
	Info  *agentv0.AgentInformation
}

// DiscoverPlugins inspects installed service agents without invoking lifecycle
// operations. An uninspectable candidate is an error: silently dropping it
// could turn an ambiguous selection into a different, apparently unique one.
func DiscoverPlugins(ctx context.Context) ([]Plugin, error) {
	installed, err := manager.Installed(ctx, resources.ServiceAgent)
	if err != nil {
		return nil, err
	}
	var plugins []Plugin
	var failures []error
	for _, selected := range installed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Preserve the manager's startup/dial budgets and bound the discovery RPC too.
		probeCtx, cancel := context.WithTimeout(ctx, manager.DefaultStartupTimeout+2*manager.DefaultDialTimeout)
		agent, info, err := services.InspectAgent(probeCtx, selected)
		cancel()
		if errors.Is(err, contract.ErrIncompatible) || errors.Is(err, manager.ErrAgentVersionMismatch) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", selected, err))
			continue
		}
		if info.GetValidation().GetSourcePackage().GetSupported() {
			plugins = append(plugins, Plugin{Agent: agent, Info: info})
		}
	}
	if err := errors.Join(failures...); err != nil {
		return nil, fmt.Errorf("cannot discover source agents; select an agent explicitly to avoid unrelated installed candidates: %w", err)
	}
	return plugins, nil
}

func selectPlugin(ctx context.Context, sourceDir string) (*resources.Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	physicalSource, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source root: %w", err)
	}
	server := codecore.NewDefaultCodeServer(physicalSource)
	defer server.Close()
	response, err := server.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_DiscoverCodeUnits{DiscoverCodeUnits: &codev0.DiscoverCodeUnitsRequest{}},
	})
	if err != nil {
		return nil, err
	}
	if failure := response.GetFailure(); failure != nil {
		return nil, fmt.Errorf("discover source units: %s", failure.GetMessage())
	}
	units := response.GetDiscoverCodeUnits().GetCodeUnits()
	var languages []string
	for _, unit := range units {
		if unit.GetPath() == "." {
			languages = unit.GetLanguages()
			break
		}
	}
	if len(languages) == 0 {
		return nil, fmt.Errorf("source root has no declared language boundary; select an agent explicitly")
	}
	plugins, err := DiscoverPlugins(ctx)
	if err != nil {
		return nil, err
	}
	return selectForLanguages(plugins, languages)
}

func selectForLanguages(plugins []Plugin, languages []string) (*resources.Agent, error) {
	var matches []*resources.Agent
	for _, plugin := range plugins {
		supported := make(map[string]bool)
		for _, language := range plugin.Info.GetLanguages() {
			if language != nil {
				supported[strings.ToLower(language.GetType().String())] = true
			}
		}
		matchesAll := len(languages) > 0
		for _, language := range languages {
			if !supported[language] {
				matchesAll = false
			}
		}
		if matchesAll {
			matches = append(matches, plugin.Agent)
		}
	}
	if len(matches) == 1 {
		selected := *matches[0]
		return &selected, nil
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no installed compatible source agent advertises languages %v; install one or select an agent explicitly", languages)
	}
	names := make([]string, len(matches))
	for i, match := range matches {
		names[i] = match.Identifier()
	}
	return nil, fmt.Errorf("multiple compatible source agents advertise languages %v: %s; select an agent explicitly", languages, strings.Join(names, ", "))
}
