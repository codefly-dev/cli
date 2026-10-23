package generate

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
)

// solutionBackend is a solution's backend seen as a contract carrier: the one
// http endpoint the host forwards the solution's routes to, and the interface
// document the backend commits for it.
//
// A solution backend declares its routes as documented operations, and the
// solution runtime renders exactly those into a Swagger 2.0 interface document
// the backend commits at core's standards.OpenAPIPath (and re-renders in CI).
// That is the same file and dialect core reads as a rest endpoint's OpenAPI
// document, so it is that endpoint's contract: the CLI reads the committed
// document and never writes a second description of the backend's routes.
type solutionBackend struct {
	// endpoint is the backend's http endpoint.
	endpoint string
	// document is the interface document the backend commits for endpoint,
	// as core's REST loader reads it; nil when the backend commits none.
	document *basev0.RestAPI
	// documentPath is where that document is, or would be, committed.
	documentPath string
}

// solutionBackendOf returns service as a solution backend, or nil when it is
// not one. A service is a solution's backend when its module ships a solution
// manifest and names it as its service-entry — the service the host forwards
// /<solution>/ to — and its one http-or-rest endpoint is http. A rest endpoint
// already carries the same document through core, so it is not taken here.
func solutionBackendOf(ctx context.Context, module *resources.Module, service *resources.Service) (*solutionBackend, error) {
	if module == nil || service == nil || module.ServiceEntry == "" || module.ServiceEntry != service.Name {
		return nil, nil
	}
	solution, err := solutionrun.ModuleSolution(module)
	if err != nil {
		return nil, fmt.Errorf("module %s: %w", module.Name, err)
	}
	if solution == nil {
		return nil, nil
	}
	var routed []*resources.Endpoint
	for _, endpoint := range service.Endpoints {
		if endpoint != nil && (endpoint.API == standards.HTTP || endpoint.API == standards.REST) {
			routed = append(routed, endpoint)
		}
	}
	if len(routed) != 1 || routed[0].API != standards.HTTP {
		return nil, nil
	}

	backend := &solutionBackend{endpoint: routed[0].Name, documentPath: filepath.Join(service.Dir(), standards.OpenAPIPath)}
	exists, err := shared.FileExists(ctx, backend.documentPath)
	if err != nil {
		return nil, fmt.Errorf("cannot check %s: %w", backend.documentPath, err)
	}
	if !exists {
		return backend, nil
	}
	document, err := resources.LoadRestAPI(ctx, &backend.documentPath)
	if err != nil {
		return nil, fmt.Errorf("solution backend %s/%s: cannot read its interface document %s: %w", module.Name, service.Name, standards.OpenAPIPath, err)
	}
	if len(document.Groups) == 0 {
		return nil, fmt.Errorf("solution backend %s/%s: its interface document %s documents no operation", module.Name, service.Name, standards.OpenAPIPath)
	}
	backend.document = document
	return backend, nil
}

// missingDocument says what a solution backend with no committed interface
// document lacks, and how it gets one.
func (b *solutionBackend) missingDocument(module, service string) string {
	return fmt.Sprintf("solution backend %s/%s commits no interface document for its %s endpoint at %s; render it from the backend's documented operations (the solution runtime's InterfaceArtifact) and commit it",
		module, service, b.endpoint, standards.OpenAPIPath)
}

// contractEndpoint is the backend's http endpoint carrying its interface
// document as REST details, the shape core gives a rest endpoint and every
// OpenAPI contract writer reads. Its api stays http: the document describes
// the endpoint, it does not change how the endpoint is reached.
func (b *solutionBackend) contractEndpoint(module, service string) *basev0.Endpoint {
	return &basev0.Endpoint{
		Name:       b.endpoint,
		Service:    service,
		Module:     module,
		Api:        standards.HTTP,
		ApiDetails: resources.ToRestAPI(b.document),
	}
}
