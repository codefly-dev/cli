package gitops

import (
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestServiceRenderDestinationsKeepDependenciesInDistinctOwnedPaths(t *testing.T) {
	resolve := serviceRenderDestinations("/render")
	api := resolve(&resources.Module{Name: "payments"}, &resources.Service{Name: "api"})
	database := resolve(&resources.Module{Name: "platform"}, &resources.Service{Name: "postgres"})
	if api != filepath.Join("/render", "modules", "payments", "services", "api") {
		t.Fatalf("origin destination = %q", api)
	}
	if database != filepath.Join("/render", "modules", "platform", "services", "postgres") {
		t.Fatalf("dependency destination = %q", database)
	}
	if api == database {
		t.Fatal("origin and dependency render destinations collide")
	}
}
