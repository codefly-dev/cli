// Command providerfixture is a test-only provider peer for the CLI's provider
// host-boundary gates. It derives its advertised runtime catalog from the
// manifest installed beside it, the way a real provider advertises the manifest
// it was packaged with, so the CLI can exercise the provider protocol without
// downloading a released agent.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/core/agents"
	providerv1 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v1"
	"github.com/codefly-dev/core/provider/artifact"
	"github.com/codefly-dev/core/provider/manifest"
)

// AdvertiseEnv makes the fixture advertise a catalog the manifest does not
// cover, so a release whose binary and manifest disagree can be exercised.
const AdvertiseEnv = "CODEFLY_PROVIDER_FIXTURE_UNDECLARED_RESOURCE"

type server struct {
	providerv1.UnimplementedProviderServer
	catalog *providerv1.RuntimeCatalog
}

func (s *server) GetProviderInformation(
	context.Context, *providerv1.GetProviderInformationRequest,
) (*providerv1.GetProviderInformationResponse, error) {
	return &providerv1.GetProviderInformationResponse{Catalog: s.catalog}, nil
}

func main() {
	catalog, err := advertisedCatalog()
	if err != nil {
		fmt.Fprintln(os.Stderr, "provider fixture:", err)
		os.Exit(1)
	}
	agents.Serve(agents.PluginRegistration{Provider: &server{catalog: catalog}})
}

func advertisedCatalog() (*providerv1.RuntimeCatalog, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(executable + artifact.InstalledManifestSuffix)
	if err != nil {
		return nil, err
	}
	packaged, err := manifest.Load(raw)
	if err != nil {
		return nil, err
	}
	catalog := &manifest.Catalog{
		SchemaVersion:       packaged.SchemaVersion,
		ProtocolVersion:     packaged.ProtocolVersion,
		StateSchemaVersions: packaged.StateSchemaVersions,
	}
	for _, descriptor := range packaged.Requests {
		digest, err := manifest.RequestDescriptorDigest(descriptor)
		if err != nil {
			return nil, err
		}
		catalog.Requests = append(catalog.Requests, manifest.CatalogRequest{ID: descriptor.ID, Digest: digest})
	}
	for _, resourceType := range packaged.ResourceTypes {
		actions := resourceType.Actions
		if os.Getenv(AdvertiseEnv) == "1" {
			actions = append(append([]string(nil), actions...), "undeclared")
		}
		catalog.ResourceTypes = append(catalog.ResourceTypes,
			manifest.CatalogResource{ID: resourceType.ID, Actions: actions})
	}
	digest, err := catalog.Digest()
	if err != nil {
		return nil, err
	}
	runtime := &providerv1.RuntimeCatalog{
		ProtocolVersion:       catalog.ProtocolVersion,
		ManifestSchemaVersion: catalog.SchemaVersion,
		StateSchemaVersions:   catalog.StateSchemaVersions,
		Digest:                digest,
	}
	for _, request := range catalog.Requests {
		runtime.Requests = append(runtime.Requests,
			&providerv1.RuntimeCatalogRequest{Id: request.ID, Digest: request.Digest})
	}
	for _, resourceType := range catalog.ResourceTypes {
		runtime.ResourceTypes = append(runtime.ResourceTypes,
			&providerv1.RuntimeCatalogResource{Id: resourceType.ID, Actions: resourceType.Actions})
	}
	return runtime, nil
}
