package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// TestRoutedEndpointsCarryTheirAddress: a manifest's endpoints are the
// module's endpoints the host routes — the ones the composition resolved an
// address for — each with that address; an endpoint nothing routes is not
// listed, and a routed endpoint of a service that does not run as the module
// is refused by name, since a host routes only to a workload of the module.
func TestRoutedEndpointsCarryTheirAddress(t *testing.T) {
	key := func(service, name, api string) string {
		return resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "crm", Service: service, Name: name, API: api})
	}
	services := []*resources.Service{
		{Name: "api", Endpoints: []*resources.Endpoint{{Name: "rest", API: "rest"}, {Name: "metrics", API: "rest"}}},
		{Name: "admin", Endpoints: []*resources.Endpoint{{Name: "http", API: "http"}}},
	}
	carriers := map[string]string{key("api", "rest", "rest"): "http://api.crm.svc.cluster.local:8080"}
	endpoints, err := routedEndpoints("crm", services, carriers, map[string]bool{"api": true})
	if err != nil {
		t.Fatal(err)
	}
	want := solutionhost.Endpoint{Name: "rest", Service: "api", API: "rest", Address: "http://api.crm.svc.cluster.local:8080"}
	if len(endpoints) != 1 || endpoints[0] != want {
		t.Fatalf("endpoints %+v, want %+v alone", endpoints, want)
	}
	// Narrowed to the workloads that run now, an identity service not started
	// yet routes nothing yet.
	if endpoints, err = routedEndpoints("crm", services, carriers, map[string]bool{"api": true}, map[string]bool{}); err != nil || len(endpoints) != 0 {
		t.Fatalf("an unstarted workload's endpoint was listed: %+v %v", endpoints, err)
	}
	carriers[key("admin", "http", "http")] = "http://admin.crm.svc.cluster.local:8080"
	if _, err = routedEndpoints("crm", services, carriers, map[string]bool{"api": true}); err == nil || !strings.Contains(err.Error(), "declare module-identity on admin") {
		t.Fatalf("a routed endpoint of a service that does not run as the module was listed: %v", err)
	}
}

// A rendered manifest this Core cannot read is REFUSED, whatever the reason —
// a document of the family the manifest replaced, a foreign schema, or a
// tampered one — never discarded and rebased onto generation 1.
func TestRenderRefusesEveryRenderedManifestItCannotRead(t *testing.T) {
	for _, schema := range []string{
		"codefly/solution-host-binding/v4", // the presence the manifest replaced
		"codefly/module-manifest/v2",       // a version this Core does not read
		"evil",                             // arbitrary
	} {
		t.Run(schema, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "modules", "crm")
			options := solutionRenderOptions(destination)
			if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("prod")), "example.prod.crm.yaml")
			delivered, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := strings.Replace(string(delivered), solutionhost.SchemaModuleManifestV1, schema, 1)
			if rewritten == string(delivered) {
				t.Fatal("the rendered manifest does not name the schema, so this test proves nothing")
			}
			if err = os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err == nil {
				t.Fatalf("a rendered manifest carrying schema %q was accepted", schema)
			}
		})
	}
}

// A manifest of this Core's schema that its rules refuse must fail too.
func TestRenderRefusesAnInvalidRenderedManifest(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("prod")), "example.prod.crm.yaml")
	delivered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// "kind: solution" occurs only inside the embedded manifest — the
	// carrier's own "kind: ConfigMap" is a different line.
	corrupted := strings.Replace(string(delivered), "kind: "+string(solutionhost.KindSolution), "kind: neither", 1)
	if corrupted == string(delivered) {
		t.Fatal("the delivered document does not name its kind, so this test proves nothing")
	}
	if err = os.WriteFile(path, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err == nil {
		t.Fatal("a rendered manifest of no kind was accepted")
	}
}
