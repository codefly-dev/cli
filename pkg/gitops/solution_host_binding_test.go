package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// testReleaseDigest is the release digest the fixtures pin: a release digest,
// distinct from every rendered-bytes and image digest in the fixtures.
const testReleaseDigest = solutionhost.ReleaseDigest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

// otherReleaseDigest is a second release of the same module: the smallest
// change that moves a presence document's generation.
const otherReleaseDigest = solutionhost.ReleaseDigest("sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")

func testHost() *environments.EnvironmentHost {
	return &environments.EnvironmentHost{
		Coordinate:       "example/prod/region-a",
		Component:        "platform-host",
		Domain:           "example",
		Audience:         "accounts",
		TrustDomain:      "cluster.example",
		EnvelopeRevision: 1,
		Delivery:         "platform/accounts/rest",
	}
}

// solutionRenderOptions is a module render that delivers one solution
// instance's workloads, with the environment naming a host.
func solutionRenderOptions(destination string) *RenderOptions {
	return &RenderOptions{
		Destination: destination,
		Module:      "crm",
		Environment: "prod",
		Namespace:   "crm",
		Promotable:  true,
		Workspace:   "example",
		Host:        testHost(),
		Units:       promotableServiceGraph("crm", []string{"api"}),
		SolutionInstances: []SolutionInstance{{
			Kind:          solutionhost.KindSolution,
			Name:          "crm",
			Alias:         "crm",
			Package:       "example/crm",
			Version:       "1.4.0",
			ReleaseDigest: testReleaseDigest,
			Units:         []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "crm@example.iam.test"}},
			Endpoints: []SolutionEndpoint{
				{Name: "grpc", Service: "api", Module: "crm", API: "grpc", Visibility: "internal"},
			},
			Modules: []SolutionModulePin{{Module: "crm", Package: "example/crm", Version: "1.4.0"}},
		}},
	}
}

// renderWorkload writes one promotable unit whose body the caller controls, so
// a test can change what the render delivers and watch the binding follow.
func renderWorkload(body string) func(context.Context, string) error {
	return func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "prod")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(body), 0o644)
	}
}

// deliveredBinding parses the document out of the ConfigMap the render wrote.
func deliveredBinding(t *testing.T, destination, binding string) *solutionhost.SolutionHostBinding {
	t.Helper()
	return deliveredBindingIn(t, destination, "prod", binding)
}

func deliveredBindingIn(t *testing.T, destination, environment, binding string) *solutionhost.SolutionHostBinding {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay(environment)), binding+".yaml"))
	if err != nil {
		t.Fatalf("read delivered binding: %v", err)
	}
	var carrier solutionHostBindingConfigMap
	if err = yaml.Unmarshal(data, &carrier); err != nil {
		t.Fatalf("decode delivered ConfigMap: %v", err)
	}
	if carrier.Kind != kindConfigMap || carrier.APIVersion != "v1" {
		t.Fatalf("delivered carrier is %s/%s, not v1/ConfigMap", carrier.APIVersion, carrier.Kind)
	}
	document, held := carrier.Data[solutionhost.FileName]
	if !held {
		t.Fatalf("delivered ConfigMap carries no %s, only %v", solutionhost.FileName, carrier.Data)
	}
	parsed, err := solutionhost.Parse([]byte(document))
	if err != nil {
		t.Fatalf("delivered document does not parse: %v", err)
	}
	return parsed
}

func TestRenderDeclaresOneBindingPerSolutionInstance(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SolutionHostBindings) != 1 {
		t.Fatalf("rendered bindings %+v", result.SolutionHostBindings)
	}
	declared := result.SolutionHostBindings[0]
	if declared.Path != "solution-host-bindings/overlays/prod/example.prod.crm.yaml" ||
		declared.Binding != "example.prod.crm" || declared.Generation != 1 {
		t.Fatalf("declared binding %+v", declared)
	}
	document := deliveredBinding(t, destination, "example.prod.crm")
	if document.Binding != "example.prod.crm" {
		t.Fatalf("binding ID %q", document.Binding)
	}
	if document.Generation != 1 {
		t.Fatalf("first generation is %d, want 1", document.Generation)
	}
	if document.Host.Coordinate != "example/prod/region-a" || document.Host.Component != "platform-host" {
		t.Fatalf("host target %+v", document.Host)
	}
	if document.Kind != solutionhost.KindSolution || document.OwnershipDomain != "example" || document.EnvelopeRevision != 1 {
		t.Fatalf("kind %q domain %q envelope revision %d", document.Kind, document.OwnershipDomain, document.EnvelopeRevision)
	}
	if len(document.Workloads) != 1 {
		t.Fatalf("workloads %+v", document.Workloads)
	}
	workload := document.Workloads[0]
	if workload.Name != "api" || workload.Artifact != "api" || workload.Container != "api" {
		t.Fatalf("workload %+v", workload)
	}
	if workload.Image.Repository != "ghcr.io/codefly-dev/api" || string(workload.Image.Digest) != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("workload image %+v", workload.Image)
	}
	if workload.Identity.Audience != "accounts" || workload.Identity.Subject != "crm@example.iam.test" ||
		workload.Identity.SPIFFEID != "spiffe://cluster.example/ns/crm/sa/api" {
		t.Fatalf("workload identity %+v", workload.Identity)
	}
	// Declared empty, never absent: core refuses a nil list because a host
	// could not tell it from "there are none".
	if workload.NonAuthenticating == nil || len(*workload.NonAuthenticating) != 0 {
		t.Fatalf("non-authenticating containers must be declared empty, got %#v", workload.NonAuthenticating)
	}
	if document.Release.Publisher != "example" || document.Release.Name != "crm" || document.Release.Version != "1.4.0" {
		t.Fatalf("release %+v", document.Release)
	}
	// The release digest is required: a generation without one can be matched
	// to no authority.
	if document.Release.Digest != testReleaseDigest {
		t.Fatalf("release digest is %q", document.Release.Digest)
	}
	if len(document.Artifacts) != 1 {
		t.Fatalf("artifacts %+v", document.Artifacts)
	}
	artifact := document.Artifacts[0]
	if artifact.Name != "api" || artifact.Release != "example/crm@1.4.0" || !strings.HasPrefix(string(artifact.Digest), "sha256:") {
		t.Fatalf("artifact %+v", artifact)
	}
	if len(document.Routes) != 1 || document.Routes[0].Alias != "crm" {
		t.Fatalf("routes %+v", document.Routes)
	}
	if len(document.Endpoints) != 1 || document.Endpoints[0].API != "grpc" {
		t.Fatalf("endpoints %+v", document.Endpoints)
	}
	if len(document.Modules) != 1 || document.Modules[0].Version != "1.4.0" {
		t.Fatalf("module pins %+v", document.Modules)
	}
	// The delivered document is also part of the render: it passed the
	// promotable ruleset and was hashed into the render digest.
	var carried bool
	for _, file := range result.Inventory.Files {
		if file.Path == "solution-host-bindings/overlays/prod/example.prod.crm.yaml" {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the binding is not in the render inventory: %+v", result.Inventory.Files)
	}
	if err = ValidateRenderedTree(destination, "", true); err != nil {
		t.Fatalf("installed tree with a binding does not validate: %v", err)
	}
}

func TestRenderedBindingAdvancesGenerationOnlyWhenTheRenderChanges(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	first := deliveredBinding(t, destination, "example.prod.crm")
	firstBytes, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// A re-render of the same desired state must not bump, and must deliver
	// byte-identical output, so the host reads it as the generation it already
	// applied rather than as a rewrite.
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	again := deliveredBinding(t, destination, "example.prod.crm")
	if again.Generation != first.Generation {
		t.Fatalf("a no-change re-render bumped the generation %d -> %d", first.Generation, again.Generation)
	}
	againBytes, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(againBytes) {
		t.Fatalf("a no-change re-render produced different bytes:\n%s\n---\n%s", firstBytes, againBytes)
	}

	// A change to a rendered artifact changes its digest, so the generation
	// advances by exactly one.
	changed := strings.Replace(pinnedDeployment, "name: api\nspec:", "name: api\n  labels:\n    changed: \"true\"\nspec:", 1)
	if changed == pinnedDeployment {
		t.Fatal("test fixture did not change")
	}
	if _, err = RenderOwnedTree(context.Background(), options, renderWorkload(changed)); err != nil {
		t.Fatal(err)
	}
	bumped := deliveredBinding(t, destination, "example.prod.crm")
	if bumped.Generation != first.Generation+1 {
		t.Fatalf("a changed render produced generation %d, want %d", bumped.Generation, first.Generation+1)
	}
	if bumped.Artifacts[0].Digest == first.Artifacts[0].Digest {
		t.Fatal("the artifact digest did not follow the delivered bytes")
	}
}

func TestBindingIDIsStablePerInstanceAndDistinctPerSecondInstance(t *testing.T) {
	first, err := bindingID("example", "prod", "crm")
	if err != nil {
		t.Fatal(err)
	}
	again, err := bindingID("example", "prod", "crm")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("binding ID is not stable: %q != %q", first, again)
	}
	// A second instance of the same solution is a second composed module under
	// a second name.
	second, err := bindingID("example", "prod", "crm-eu")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("a second instance reuses binding ID %q", first)
	}
	// Two workspaces delivering to one host must not claim the same binding.
	other, err := bindingID("acme", "prod", "crm")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatalf("a second workspace reuses binding ID %q", first)
	}
	for _, id := range []string{first, second, other} {
		if len(id) > bindingIDMaxLength {
			t.Fatalf("binding ID %q exceeds a label value", id)
		}
	}
	if _, err = bindingID("example", "prod", strings.Repeat("a", 64)); err == nil {
		t.Fatal("an over-long binding ID was accepted")
	}
	if _, err = bindingID("example", "prod", "crm.eu"); err == nil {
		t.Fatal("an instance name carrying the join separator was accepted")
	}
}

func TestRenderRefusesARouteAliasAHostCannotKeyOn(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	// core's namePattern admits it; a host keys its registry on the alias as a
	// single URL path segment, and a slashed alias cannot be one.
	options.SolutionInstances[0].Alias = "example/crm"
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "URL path segment") {
		t.Fatalf("a slashed route alias was accepted: %v", err)
	}
}

func TestRenderRefusesASolutionWhoseWorkloadAuthenticatesAsNothing(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.SolutionInstances[0].Units[0].Subject = ""
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "declares no workload identity") {
		t.Fatalf("a binding with no subject was rendered: %v", err)
	}
}

// TestRenderRefusesACompositionWithNoHost: an environment that composes a
// module declares the host it runs on. Rendering the instances' workloads
// with no declaration delivered them present on no host, with a warning the
// operator could miss; the render refuses instead, naming the instances and
// the host block's fields.
func TestRenderRefusesACompositionWithNoHost(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.Host = nil
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "composes crm but declares no host block") {
		t.Fatalf("a composition with no host must be refused by name, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destination, InventoryFilename)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused render must leave no tree behind: %v", statErr)
	}
}

func TestRenderRefusesAnUnreadableDeliveredBinding(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	if _, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\ndata: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Falling back to generation 1 here would deliver a document every host
	// that applied this binding refuses, and say nothing about it.
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "carries no "+solutionhost.FileName) {
		t.Fatalf("an unreadable delivered binding was ignored: %v", err)
	}
}

// --- core's shipped conformance fixtures, as negative tests ---
//
// The CLI's gate is core's own AdmitRendered over the set the render is about
// to write — the renderer's share of admission, over parsed documents, since a
// host's Admit takes documents whose carrier was verified and a render has not
// produced one yet. Driving it with the documents core ships is what proves
// the gate is the contract rather than the CLI's reading of it.

func TestRenderedSetAdmissionRefusesCoreFixtures(t *testing.T) {
	fixtures := map[string]solutionhost.Fixture{}
	for _, fixture := range solutionhost.Fixtures() {
		fixtures[fixture.Name] = fixture
	}
	for _, name := range []string{"duplicate-route-alias", "mixed-release"} {
		fixture, shipped := fixtures[name]
		if !shipped {
			t.Fatalf("core ships no %q fixture", name)
		}
		if fixture.Outcome != solutionhost.OutcomeRejected {
			t.Fatalf("fixture %q is not a rejection: %s", name, fixture.Outcome)
		}
		t.Run(name, func(t *testing.T) {
			// mixed-release is refused by the document's own rules, so Parse
			// already rejects it; duplicate-route-alias parses and is refused
			// only against the set. Both reach the renderer's gate the same
			// way: the set it is about to write is never admitted.
			document, err := solutionhost.Parse(fixture.Document)
			if err != nil {
				if name == "mixed-release" && !errors.Is(err, solutionhost.ErrMixedRelease) {
					t.Fatalf("mixed-release was refused for the wrong reason: %v", err)
				}
				return
			}
			valid, err := solutionhost.Parse(mustFixtureDocument(t, "valid"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = solutionhost.AdmitRendered(valid, document); err == nil {
				t.Fatalf("the renderer's gate admitted %q", name)
			} else if name == "duplicate-route-alias" && !errors.Is(err, composition.ErrCollision) {
				t.Fatalf("duplicate-route-alias was refused for the wrong reason: %v", err)
			}
		})
	}
}

// TestRenderRefusesTheSetItIsAboutToWrite drives the refusal through the render
// itself: two instances of the same solution that end up claiming one alias are
// never written, and the tree keeps whatever it held.
func TestRenderRefusesAColludingRenderedSet(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	second := options.SolutionInstances[0]
	second.Name = "crm-eu"
	// Two instances, one alias: exactly core's duplicate-route-alias case,
	// refused where it was authored instead of on the host.
	options.SolutionInstances = append(options.SolutionInstances, second)
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "not admissible") {
		t.Fatalf("a colliding rendered set was written: %v", err)
	}
	if !errors.Is(err, composition.ErrCollision) {
		t.Fatalf("the collision did not carry composition.ErrCollision: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destination, solutionHostBindingDir)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused render left documents behind: %v", statErr)
	}
}

func TestRenderRefusesAMixedReleaseSet(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	// An artifact rendered from another release cannot be declared in this
	// generation: a partial rollout is not a thing delivery may describe.
	options.SolutionInstances[0].Units = append(options.SolutionInstances[0].Units,
		SolutionArtifactUnit{Name: "worker", Path: "services/worker", Subject: "crm@example.iam.test"})
	options.SolutionInstances[0].Package = "example/crm"
	options.Units = promotableServiceGraph("crm", []string{"api", "worker"})
	result, err := RenderOwnedTree(context.Background(), options, func(ctx context.Context, root string) error {
		if err := renderWorkload(pinnedDeployment)(ctx, root); err != nil {
			return err
		}
		overlay := filepath.Join(root, "services", "worker", "overlays", "prod")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(strings.ReplaceAll(strings.ReplaceAll(pinnedDeployment, "name: api", "name: worker"), "codefly-dev/api@", "codefly-dev/worker@")), 0o644)
	})
	if err != nil {
		t.Fatalf("two artifacts of one release were refused: %v (%+v)", err, result.SolutionHostBindings)
	}
	document := deliveredBinding(t, destination, "example.prod.crm")
	for _, artifact := range document.Artifacts {
		if artifact.Release != document.Release.Identity() {
			t.Fatalf("artifact %s names release %q, not %q", artifact.Name, artifact.Release, document.Release.Identity())
		}
	}
}

func mustFixtureDocument(t *testing.T, name string) []byte {
	t.Helper()
	data, err := solutionhost.FixtureDocument(solutionhost.DocumentTypePresence, name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// loadHostedSolutionWorkspace is loadSolutionWorkspace with the environment
// naming the host it delivers to and the identity its solution workloads
// present — the two declarations a binding cannot be rendered without.
func loadHostedSolutionWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	config := `name: hello
layout: flat
environments:
  - name: local
    namespace: hello
    cluster:
      kind: k3d
    host:
      coordinate: example/local/dev
      component: platform-host
      domain: example
      audience: accounts
      trust_domain: cluster.local
      envelope_revision: 1
      delivery: hello/host/rest
    service-identity:
      default:
        principal: lastlogin@example.iam.test
`
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

// TestRenderSolutionDeclaresItsOwnPresence drives the packaged-solution render
// end to end through an in-process executor and checks that the binding is
// generated from the same resolution the workloads were: the executor's own
// artifact identity is the release, and the unit it wrote is the artifact the
// document pins.
func TestRenderSolutionDeclaresItsOwnPresence(t *testing.T) {
	installFakeSolutionExecutor(t, &fakeSolutionExecutor{})
	workspace := loadHostedSolutionWorkspace(t)
	env := selectedEnvironment(t, workspace, "local")
	if env == nil {
		t.Fatal("environment local not found")
	}
	result, err := RenderSolution(context.Background(), &SolutionRenderRequest{
		Workspace:   workspace,
		Environment: env,
		Agent:       &resources.Agent{Kind: resources.SolutionAgent, Publisher: "example", Name: "lastlogin", Version: "0.0.1"},
		Name:        "lastlogin-go",
		Source:      filepath.Join(workspace.Dir(), "solution-src"),
		Reference:   "ghcr.io/codefly-dev/hello-solution:0.0.1",
		AppProject:  "hello",
	})
	if err != nil {
		t.Fatalf("RenderSolution: %v", err)
	}
	if len(result.SolutionHostBindings) != 1 {
		t.Fatalf("rendered bindings %+v", result.SolutionHostBindings)
	}
	document := deliveredBindingIn(t, result.Path, "local", "hello.local.lastlogin-go")
	if document.Host.Coordinate != "example/local/dev" {
		t.Fatalf("host %+v", document.Host)
	}
	if document.Release.Identity() != "example/lastlogin@0.0.1" {
		t.Fatalf("release identity %q", document.Release.Identity())
	}
	if len(document.Routes) != 1 || document.Routes[0].Alias != "lastlogin-go" {
		t.Fatalf("routes %+v", document.Routes)
	}
	if len(document.Artifacts) != 1 || document.Artifacts[0].Name != "lastlogin-go" {
		t.Fatalf("artifacts %+v", document.Artifacts)
	}
	if len(document.Workloads) != 1 || document.Workloads[0].Identity.Subject != "lastlogin@example.iam.test" {
		t.Fatalf("workloads %+v", document.Workloads)
	}
	if document.Release.Digest != "sha256:"+digestPlaceholder {
		t.Fatalf("release digest %q is not the artifact digest the executor reported", document.Release.Digest)
	}
}

// TestDeliveredCarrierMatchesTheGolden pins the whole carrier, byte for byte:
// the ConfigMap the host selects on, the data key it reads the document from,
// and the document itself. It is the shape another repository's reconciler is
// written against, so a change to any of it is a change to a contract and has
// to be seen in a diff.
//
// It also guards the one thing a Go map in the carrier could break silently:
// map iteration order is randomized per process, so labels encoded unsorted
// would make every re-render a rewrite of a generation the host already
// applied, visible only as an intermittent ErrRewrittenGeneration in
// production.
func TestDeliveredCarrierMatchesTheGolden(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(pinnedDeployment)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "solution-host-binding.golden.yaml")
	golden, err := os.ReadFile(goldenPath)
	if os.IsNotExist(err) && os.Getenv("UPDATE_GOLDEN") != "" {
		if err = os.WriteFile(goldenPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(golden) != string(body) {
		t.Fatalf("the delivered carrier changed:\nwant:\n%s\ngot:\n%s", golden, body)
	}
}

// TestDeclaredBindingsReachArgo is the delivery half, and it is the failure
// this test exists for: Argo points an Application at each overlay the
// ApplicationSet names, so a binding written anywhere the ApplicationSet does
// not name is committed to the delivery repository and applied to nothing —
// the render reports success, the tree looks right, and the host never sees a
// document. It also checks the derived AppProject authority covers it, since
// an Application whose resource kind the project does not whitelist is
// refused at sync.
func TestDeclaredBindingsReachArgo(t *testing.T) {
	root := t.TempDir()
	targetPath := "environments/deployments/modules/crm"
	inventory := &Inventory{
		SchemaVersion: SchemaVersion,
		Module:        "crm",
		Environment:   "prod",
		Namespace:     "crm",
		AppProject:    "crm",
		Units: []InventoryUnit{
			{Kind: UnitKindService, Module: "crm", Name: "api", Path: "services/api"},
		},
		SolutionHostBindingPath: solutionHostBindingDir,
	}
	writeOverlay(t, filepath.Join(root, "services", "api", "overlays", "prod"))
	writeOverlay(t, filepath.Join(root, solutionHostBindingDir, "overlays", "prod"))
	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	if err := generateArgoBootstrap(
		context.Background(), config, root, targetPath, inventory, "prod", strings.Repeat("c", 40), "",
	); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "bootstrap", "applicationset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set := string(data)
	overlay := targetPath + "/" + solutionHostBindingDir + "/overlays/prod"
	if !strings.Contains(set, "overlay: "+overlay) {
		t.Fatalf("no Argo Application delivers the declared bindings:\n%s", set)
	}
	// After every unit of the module: the host is a module of the composition
	// too, and under a parent that syncs by wave its own delivery must not
	// wait on a service ordered after it.
	if !strings.Contains(set, "wave: \""+deliveryWave+"\"") {
		t.Fatalf("the bindings do not land in the delivery wave:\n%s", set)
	}
	if deliveryWave <= consumerUnitWave {
		t.Fatalf("delivery is ordered before the units it may depend on (wave %q)", deliveryWave)
	}
	cluster, namespaced, err := snapshotAuthority(root, inventory, "prod")
	if err != nil {
		t.Fatalf("derive promotion authority over a tree carrying bindings: %v", err)
	}
	var covered bool
	for _, authority := range namespaced {
		if authority.Kind == kindConfigMap {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("the AppProject does not admit the binding's ConfigMap: cluster=%+v namespaced=%+v", cluster, namespaced)
	}
}

// TestRenderedTreeDeliversItsBindings closes the loop end to end: what the
// render actually wrote is what the ApplicationSet points an Application at.
func TestRenderedTreeDeliversItsBindings(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.AppProject = "crm"
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	if result.Inventory.SolutionHostBindingPath != solutionHostBindingDir {
		t.Fatalf("the inventory does not record the delivered binding path: %q", result.Inventory.SolutionHostBindingPath)
	}
	overlay := filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")))
	// The overlay must build: Argo applies what the kustomization names, so a
	// document missing from it is delivered nowhere.
	kustomization, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("the delivered binding overlay has no kustomization: %v", err)
	}
	if !strings.Contains(string(kustomization), "example.prod.crm.yaml") {
		t.Fatalf("the kustomization does not name the delivered document:\n%s", kustomization)
	}
	for _, declared := range result.SolutionHostBindings {
		if !strings.HasPrefix(declared.Path, solutionHostBindingOverlay("prod")+"/") {
			t.Fatalf("binding %q is outside the overlay Argo delivers", declared.Path)
		}
	}
}

// TestInventoryRoundTripKeepsTheDeliveryPath guards the silent drop: anything
// that re-derives an inventory over an already-rendered tree (a dev
// deployment) must carry the declared binding path back, or the documents stay
// in the tree while the next publish stops delivering them — a change with no
// diff in the manifests and none in the render digest, since the digest covers
// files and not inventory fields.
// TestPresenceNamesOnlyTheWorkloadsThatMint: a bootstrap Job of a unit runs
// its own image and never authenticates as the service, so the presence
// document names the serving workloads and not it — listed, its image would
// be one the host accepts a token from. The cell still inventories the Job
// for admission, so the two files describe the same pods for different ends.
func TestPresenceNamesOnlyTheWorkloadsThatMint(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(cellDeployment))
	if err != nil {
		t.Fatal(err)
	}
	document := deliveredBinding(t, destination, "example.prod.crm")
	if len(document.Workloads) != 1 {
		t.Fatalf("presence workloads %+v, want the Deployment alone", document.Workloads)
	}
	workload := document.Workloads[0]
	if workload.Name != "api" || workload.Container != "api" {
		t.Fatalf("presence names %s/%s, want the serving Deployment api", workload.Name, workload.Container)
	}
	for _, build := range document.Builds() {
		if strings.Contains(string(build), strings.Repeat("d", 64)) {
			t.Fatalf("the migrate Job's image is an approved build: %v", document.Builds())
		}
	}
}

func TestInventoryRoundTripKeepsTheDeliveryPath(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	result, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := buildInventory(destination, inventoryRenderOptions(&result.Inventory))
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.SolutionHostBindingPath != result.Inventory.SolutionHostBindingPath {
		t.Fatalf("re-deriving the inventory dropped the delivery path: %q became %q",
			result.Inventory.SolutionHostBindingPath, rebuilt.SolutionHostBindingPath)
	}
}

func TestReleaseIdentityRefusesAPackageItCannotName(t *testing.T) {
	for name, instance := range map[string]SolutionInstance{
		"no publisher": {Package: "crm", Version: "1.0.0"},
		"empty":        {Package: "", Version: "1.0.0"},
		"three parts":  {Package: "example/crm/eu", Version: "1.0.0"},
		"empty name":   {Package: "example/", Version: "1.0.0"},
		"no version":   {Package: "example/crm", Version: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := releaseIdentity(&instance); err == nil {
				t.Fatalf("release %+v was named", instance)
			}
		})
	}
	publisher, name, err := releaseIdentity(&SolutionInstance{Package: "example/crm", Version: "1.4.0"})
	if err != nil || publisher != "example" || name != "crm" {
		t.Fatalf("releaseIdentity = %q, %q, %v", publisher, name, err)
	}
}

// TestRenderingAnotherEnvironmentResetsTheGeneration pins the sharp edge of
// taking the prior generation from the delivered tree. The render destination
// is per module and NOT per environment, and a render replaces it whole, so
// rendering staging and then production again finds no prior production
// document and starts over at 1 — which the production host correctly refuses
// as stale.
//
// It is a test rather than a note because the behaviour is load-bearing: the
// alternative, inventing a generation the renderer cannot know, is worse. The
// render reports the generation it declared so the reset is visible where it
// happens.
func TestRenderingAnotherEnvironmentResetsTheGeneration(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	production := solutionRenderOptions(destination)
	changed := strings.Replace(pinnedDeployment, "name: api\nspec:", "name: api\n  labels:\n    v: \"2\"\nspec:", 1)
	for _, body := range []string{pinnedDeployment, changed} {
		if _, err := RenderOwnedTree(context.Background(), production, renderWorkload(body)); err != nil {
			t.Fatal(err)
		}
	}
	if advanced := deliveredBinding(t, destination, "example.prod.crm").Generation; advanced != 2 {
		t.Fatalf("production reached generation %d, want 2", advanced)
	}

	staging := solutionRenderOptions(destination)
	staging.Environment = "staging"
	staging.SolutionInstances[0].Units[0].Path = "services/api"
	stagingRender := func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(pinnedDeployment), 0o644)
	}
	if _, err := RenderOwnedTree(context.Background(), staging, stagingRender); err != nil {
		t.Fatal(err)
	}
	// The whole owned tree was replaced, production's document with it.
	if _, err := os.Stat(filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay("prod")))); !os.IsNotExist(err) {
		t.Fatalf("production's overlay survived a staging render: %v", err)
	}
	result, err := RenderOwnedTree(context.Background(), production, renderWorkload(changed))
	if err != nil {
		t.Fatal(err)
	}
	if got := deliveredBinding(t, destination, "example.prod.crm").Generation; got != 1 {
		t.Fatalf("production resumed at generation %d; the prior document was gone, so 1 is the honest answer", got)
	}
	// And the render says so, rather than leaving the reset to be discovered
	// when the host refuses the document.
	if len(result.SolutionHostBindings) != 1 || result.SolutionHostBindings[0].Generation != 1 {
		t.Fatalf("the render did not report the generation it declared: %+v", result.SolutionHostBindings)
	}
}

// TestBindingIDRefusesAPartThatCannotNameAKubernetesObject is the guard whose
// absence let an invalid ConfigMap name reach ArgoCD.
//
// core's bindingPattern deliberately admits uppercase and underscores so a ULID
// or a UUID can be a binding ID, so core's own Validate accepts
// "My_Workspace.prod.crm" — and the render would then deliver a ConfigMap named
// "solution-host-binding-My_Workspace.prod.crm", which Kubernetes refuses as an
// object name. Nothing downstream catches it: the tree validates and the
// publish succeeds, so the first refusal is ArgoCD at sync, far from here.
func TestBindingIDRefusesAPartThatCannotNameAKubernetesObject(t *testing.T) {
	for name, part := range map[string]struct{ workspace, environment, instance string }{
		"uppercase workspace":   {"My_Workspace", "prod", "crm"},
		"capitalised":           {"Obin", "prod", "crm"},
		"underscore":            {"obin_prod", "prod", "crm"},
		"uppercase instance":    {"obin", "prod", "CRM"},
		"leading dash":          {"obin", "prod", "-crm"},
		"trailing dash":         {"obin", "prod", "crm-"},
		"uppercase environment": {"obin", "Prod", "crm"},
	} {
		t.Run(name, func(t *testing.T) {
			id, err := bindingID(part.workspace, part.environment, part.instance)
			if err == nil {
				t.Fatalf("binding ID %q was accepted; it would name the ConfigMap %q", id, solutionHostBindingKind+"-"+id)
			}
			if !strings.Contains(err.Error(), "Kubernetes object") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
	if _, err := bindingID("example", "prod", "crm-eu-1"); err != nil {
		t.Fatalf("a legal binding ID was refused: %v", err)
	}
}

// TestRenderRefusesAWorkspaceThatCannotNameAKubernetesObject drives the same
// refusal through the render, so the guard is exercised where it matters and
// not only in the helper.
func TestRenderRefusesAWorkspaceThatCannotNameAKubernetesObject(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	options.Workspace = "My_Workspace"
	_, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err == nil || !strings.Contains(err.Error(), "Kubernetes object") {
		t.Fatalf("the render delivered a document ArgoCD would refuse: %v", err)
	}
}

// TestARenderThatDeclaresNothingClearsTheDeliveryPath is the guard whose
// absence refused an entire publication.
//
// The inventory field is what points publish at the binding overlay. Writing it
// only when a binding was written leaves a previous render's value in place on
// a reused RenderOptions, and both readers of the field kustomize-build
// "<path>/overlays/<environment>" — so a stale value is not a cosmetic
// inaccuracy, it fails generateArgoBootstrap and refuses the whole publication.
func TestARenderThatDeclaresNothingClearsTheDeliveryPath(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	options := solutionRenderOptions(destination)
	first, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	if first.Inventory.SolutionHostBindingPath != solutionHostBindingDir {
		t.Fatalf("the first render recorded no delivery path: %q", first.Inventory.SolutionHostBindingPath)
	}
	// The module stops composing a solution instance and re-renders with the
	// same options, as a caller that holds one render's options would.
	options.SolutionInstances = nil
	second, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	if err != nil {
		t.Fatal(err)
	}
	if second.Inventory.SolutionHostBindingPath != "" {
		t.Fatalf("the inventory still claims %q; the tree has no such directory, and publish builds it",
			second.Inventory.SolutionHostBindingPath)
	}
	// The field and the tree must agree: the directory really is gone, so a
	// non-empty field would point publish at nothing. (What publish then does
	// with a path that is present is covered by TestDeclaredBindingsReachArgo.)
	if _, statErr := os.Stat(filepath.Join(destination, solutionHostBindingDir)); !os.IsNotExist(statErr) {
		t.Fatalf("the binding directory survived a render that declared none: %v", statErr)
	}
}
