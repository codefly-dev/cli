package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/provider/conformance"
	"github.com/codefly-dev/core/agents/manager"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/artifact"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/resources"
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
	catalog, runErr := exerciseProviderArtifact(ctx, temporary, agentDir, agent)
	if runErr != nil {
		return nil, conformanceDir, runErr
	}
	evidence, runErr := conformance.Qualify(ctx, providerManifest, declared)
	if runErr != nil {
		return nil, conformanceDir, runErr
	}
	evidence.CatalogDigest = catalog.GetDigest()
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

// exerciseProviderArtifact starts the built provider and holds its advertised
// runtime catalog to the reviewed manifest. This is the half a manifest can
// never establish: that the release actually starts, serves the provider
// protocol, and implements the requests and resource types it was reviewed on.
//
// The artifact envelope the loader requires is assembled here from the built
// binary and the shipped manifest. Its digests are derived from those same
// bytes, so the envelope is not itself evidence — the runtime catalog is.
func exerciseProviderArtifact(
	ctx context.Context,
	temporary, agentDir string,
	agent *agentYAML,
) (*providerv0.RuntimeCatalog, error) {
	identity := &resources.Agent{
		Kind:      resources.ProviderAgent,
		Publisher: agent.Publisher,
		Name:      agent.Name,
		Version:   agent.Version,
	}
	verified, err := installProviderArtifact(ctx, temporary, agentDir, identity)
	if err != nil {
		return nil, err
	}
	connection, err := manager.Load(ctx, identity,
		// Providers carry no launch layer that applies their manifest sandbox,
		// so the loader is composed the way Core composes it for a provider.
		manager.WithoutSandbox(),
		manager.WithoutPrincipal(),
	)
	if err != nil {
		return nil, fmt.Errorf("provider conformance: start the built provider: %w", err)
	}
	defer connection.Close()

	information, err := providerv0.NewProviderClient(connection.GRPCConn()).
		GetProviderInformation(ctx, &providerv0.GetProviderInformationRequest{})
	if err != nil {
		return nil, fmt.Errorf("provider conformance: read the provider advertisement: %w", err)
	}
	if _, err := verified.AdmitRuntimeCatalog(information.GetCatalog()); err != nil {
		return nil, fmt.Errorf("provider conformance: the running provider does not implement its reviewed manifest: %w", err)
	}
	return information.GetCatalog(), nil
}

// installProviderArtifact writes the binary, manifest and descriptor layout the
// provider loader verifies before it will start a provider.
func installProviderArtifact(
	ctx context.Context,
	temporary, agentDir string,
	identity *resources.Agent,
) (*artifact.Verified, error) {
	target, err := identity.Path(ctx)
	if err != nil {
		return nil, err
	}
	binary, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("read the built provider: %w", err)
	}
	registration, err := resources.AgentKindRegistrationFor(resources.ProviderAgent)
	if err != nil {
		return nil, err
	}
	// The descriptor must cover exactly the manifest bytes the release ships,
	// not a re-serialization of the parsed form.
	manifestBytes, err := os.ReadFile(filepath.Join(agentDir, providerManifestName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", providerManifestName, err)
	}
	layout := filepath.Join(temporary, "provider-artifact")
	if mkdirErr := os.MkdirAll(layout, 0o755); mkdirErr != nil {
		return nil, mkdirErr
	}
	executable := registration.ExecutableName(identity.Name)
	descriptor, err := artifact.BuildDescriptor(executable, binary, manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("describe the built provider: %w", err)
	}
	descriptorBytes, err := artifact.MarshalDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	for path, payload := range map[string][]byte{
		filepath.Join(layout, executable):                  binary,
		filepath.Join(layout, manifest.FileName):           manifestBytes,
		filepath.Join(layout, artifact.DescriptorFileName): descriptorBytes,
	} {
		mode := os.FileMode(0o644)
		if filepath.Base(path) == executable {
			mode = 0o755
		}
		if err := atomicWrite(path, payload, mode); err != nil {
			return nil, err
		}
	}
	return artifact.InstallLayout(layout, target, identity)
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
