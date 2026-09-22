package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/provider/conformance"
	"github.com/codefly-dev/core/provider/manifest"
	"gopkg.in/yaml.v3"
)

// providerManifestName is the manifest a provider release ships beside its
// binary. It is the artifact the host admits, so it is what qualification runs.
const providerManifestName = "provider.codefly.yaml"

// runProviderConformance qualifies the built provider through the host's own
// provider boundary: its shipped manifest is admitted by the real loader, and
// every owner-declared operation is composed into the exact request the host
// would plan and run through the real broker.
func runProviderConformance(ctx context.Context, temporary, agentDir string, agent *agentYAML) ([]byte, string, error) {
	conformanceDir := filepath.Join(temporary, "provider-conformance")
	declared, err := loadProviderConformanceFixture(agentDir, agent.Conformance.Fixture)
	if err != nil {
		return nil, "", err
	}
	providerManifest, err := loadProviderManifest(agentDir, agent)
	if err != nil {
		return nil, "", err
	}
	evidence, runErr := conformance.Qualify(ctx, providerManifest, declared)
	if runErr != nil {
		return nil, conformanceDir, runErr
	}
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return nil, conformanceDir, err
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(conformanceDir, 0o755); err != nil {
		return nil, conformanceDir, err
	}
	if err := atomicWrite(filepath.Join(conformanceDir, agentCIReportFilename), payload, 0o644); err != nil {
		return nil, conformanceDir, err
	}
	return payload, conformanceDir, nil
}

// loadProviderManifest reads the shipped manifest through the host parser and
// keeps qualification on the artifact agent CI just built.
func loadProviderManifest(agentDir string, agent *agentYAML) (*manifest.Manifest, error) {
	payload, err := os.ReadFile(filepath.Join(agentDir, providerManifestName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", providerManifestName, err)
	}
	loaded, err := manifest.Load(payload)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", providerManifestName, err)
	}
	target := loaded.Agent
	if target.Publisher != agent.Publisher || target.Name != agent.Name || target.Version != agent.Version {
		return nil, fmt.Errorf("%s selects %s, but the candidate is %s/%s:%s",
			providerManifestName, target.Identifier(), agent.Publisher, agent.Name, agent.Version)
	}
	return loaded, nil
}

func loadProviderConformanceFixture(agentDir, fixture string) (conformance.Declaration, error) {
	path := fixture
	if !filepath.IsAbs(path) {
		path = filepath.Join(agentDir, fixture)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return conformance.Declaration{}, fmt.Errorf("read provider conformance fixture %q: %w", fixture, err)
	}
	var declared conformance.Declaration
	if err := yaml.Unmarshal(payload, &declared); err != nil {
		return conformance.Declaration{}, fmt.Errorf("parse provider conformance fixture %q: %w", fixture, err)
	}
	if len(declared.Operations) == 0 {
		return conformance.Declaration{}, fmt.Errorf("provider conformance fixture %q declares no operation", fixture)
	}
	names := map[string]bool{}
	for index, operation := range declared.Operations {
		if strings.TrimSpace(operation.Name) == "" || strings.TrimSpace(operation.Request) == "" {
			return conformance.Declaration{}, fmt.Errorf("provider conformance fixture %q operation %d requires a name and a request", fixture, index)
		}
		if names[operation.Name] {
			return conformance.Declaration{}, fmt.Errorf("provider conformance fixture %q repeats operation %q", fixture, operation.Name)
		}
		names[operation.Name] = true
	}
	return declared, nil
}
