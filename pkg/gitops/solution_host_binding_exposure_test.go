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

// An endpoint declares TWO independent axes since core v0.14.0: visibility is
// reach and exposure is addressing. The presence document is what a host reads
// to decide both, so a projection that carries one and drops the other is a
// lossy copy of the module's declaration — and silently lossy, which is why
// these tests assert the axis survives each hop rather than asserting the
// document merely parses.

// The first hop: the loaded resource model into the renderer's own carrier.
// Tested separately from the document because the carrier is a different
// struct in a different file, and a field missing HERE cannot be seen at all
// further down — the document would be valid and wrong.
func TestPresenceInstanceCarriesTheExposureTheModuleDeclared(t *testing.T) {
	module := solutionModule(t, "crm", "api")
	services := []*resources.Service{{
		Name: "api",
		Endpoints: []*resources.Endpoint{
			{Name: "grpc", API: "grpc", Visibility: "internal"},
			{Name: "rest", API: "rest", Visibility: "public", Exposure: "public"},
			{Name: "metrics", API: "rest", Visibility: "public", Exposure: "none"},
		},
	}}
	options := &RenderOptions{
		Module: "crm", Environment: "prod", Workspace: "obin",
		Package: &InventoryPackage{ID: "obin/crm", Version: "1.4.0"},
		Units:   promotableServiceGraph("crm", []string{"api"}),
	}
	instance, undeclared, err := presenceInstanceOf(module, services, identityEnvironment(), options)
	if err != nil {
		t.Fatal(err)
	}
	if instance == nil {
		t.Fatalf("the solution instance did not resolve: %s", undeclared)
	}
	want := map[string]string{"grpc": "", "rest": "public", "metrics": "none"}
	if len(instance.Endpoints) != len(want) {
		t.Fatalf("endpoints %+v", instance.Endpoints)
	}
	for _, endpoint := range instance.Endpoints {
		expected, named := want[endpoint.Name]
		if !named {
			t.Fatalf("unexpected endpoint %q", endpoint.Name)
		}
		if endpoint.Exposure != expected {
			t.Fatalf("endpoint %q carries exposure %q, the module declared %q", endpoint.Name, endpoint.Exposure, expected)
		}
	}
}

// The second hop: the carrier into the delivered document. A host renders an
// outward address from the exposure it reads here, so this is the hop whose
// loss would be visible as a missing ingress and diagnosed as a deployment
// bug rather than a dropped field.
func TestTheDeliveredDocumentCarriesEachEndpointsExposure(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.SolutionInstances[0].Endpoints = []SolutionEndpoint{
		{Name: "grpc", Service: "api", Module: "crm", API: "grpc", Visibility: "internal"},
		{Name: "rest", Service: "api", Module: "crm", API: "rest", Visibility: "public", Exposure: "public"},
		{Name: "metrics", Service: "api", Module: "crm", API: "rest", Visibility: "public", Exposure: "none"},
	}
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	document := readDeliveredPresence(t, destination, "prod", "example.prod.crm")
	want := map[string]string{"grpc": "", "rest": "public", "metrics": "none"}
	for _, endpoint := range document.Endpoints {
		if endpoint.Exposure != want[endpoint.Name] {
			t.Fatalf("delivered endpoint %q carries exposure %q, the module declared %q", endpoint.Name, endpoint.Exposure, want[endpoint.Name])
		}
	}
	// Proven through core's own reader rather than a YAML string match, so the
	// assertion is about the document a host will read.
	if err := document.Validate(); err != nil {
		t.Fatalf("the delivered document is not one core reads: %v", err)
	}
}

// A delivered document this Core cannot read is REFUSED, whatever the reason —
// an older spelling of our own schema, a foreign document, or a tampered one.
// There is no "merely old, discard it" path: core's ErrSchema says only "this
// Core does not read this", never "an earlier version of us wrote it", so a
// reader that treated every ErrSchema as superseded would let a document whose
// schema value was changed to anything at all discard the one it replaces and
// restart the generation history on it.
//
// Nothing is lost by refusing: no delivery tree holds a presence document under
// any earlier schema, because the first render had not happened when the schema
// collapsed onto v1. A tree that somehow did would be told, loudly, rather than
// silently rebased onto generation 1.
func TestRenderRefusesEveryDeliveredDocumentItCannotRead(t *testing.T) {
	for _, schema := range []string{
		"codefly/solution-host-binding/v2",   // an earlier spelling of our own
		"codefly/solution-host-authority/v1", // a sibling document, not this one
		"evil",                               // arbitrary
	} {
		t.Run(schema, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "modules", "crm")
			options := solutionRenderOptions(destination)
			if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml")
			delivered, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := strings.Replace(string(delivered), solutionhost.SchemaPresenceV1, schema, 1)
			if rewritten == string(delivered) {
				t.Fatal("the delivered document does not name the schema, so this test proves nothing")
			}
			if err = os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err == nil {
				t.Fatalf("a delivered document carrying schema %q was accepted", schema)
			}
		})
	}
}

// A document that is NOT merely older — one this Core reads and refuses — must
// still fail. Without this, the skew branch above could be widened to swallow
// a corrupt delivery and nothing would notice.
func TestRenderStillRefusesADeliveredDocumentItCanRead(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml")
	delivered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// "kind: solution" occurs only inside the embedded document — the carrier's
	// own "kind: ConfigMap" is a different line. Anchoring on the document's
	// binding instead would match the carrier's codefly.dev/binding LABEL
	// first and mutate nothing a reader checks, which is a negative control
	// that passes by accident.
	corrupted := strings.Replace(string(delivered), "kind: "+string(solutionhost.KindSolution), "kind: neither", 1)
	if corrupted == string(delivered) {
		t.Fatal("the delivered document does not name its kind, so this test proves nothing")
	}
	if err = os.WriteFile(path, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err == nil {
		t.Fatal("a delivered document naming another binding was accepted as skew")
	}
}

// deliveredPresence reads one delivered presence document back through core's
// own reader.
func readDeliveredPresence(t *testing.T, destination, environment, binding string) *solutionhost.SolutionHostBinding {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay(environment)), binding+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, _, _, ok, err := bindingFromConfigMap(data)
	if err != nil || !ok {
		t.Fatalf("read the delivered document: ok=%v err=%v", ok, err)
	}
	return document
}
