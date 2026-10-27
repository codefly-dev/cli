package sync

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/conformance/conformancetest"
	"github.com/codefly-dev/core/composition"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/standards"
)

// fixtureProducers are testdata/solution-sdk's two composed packages: the
// consumed service, the protobuf package its proto declares, and its service.
// The solution consumes accounts over connect and records over rest; each
// catalog carries its protobuf contract under grpc, as `generate contracts`
// exports a service whose transport endpoints are not in its interface.
var fixtureProducers = []struct{ module, pkg, service string }{
	{"accounts", "fixture.accounts.v1", "AccountsService"},
	{"records", "fixture.records.v1", "RecordsService"},
}

// newTwoModuleSolution copies testdata/solution-sdk into a temp workspace and
// writes each producer's contract catalog, with contract(t, protoDir) as the
// grpc endpoint's descriptor set.
func newTwoModuleSolution(t *testing.T, contract func(t *testing.T, protoDir string) []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "solution-sdk"))); err != nil {
		t.Fatal(err)
	}
	for _, producer := range fixtureProducers {
		moduleDir := filepath.Join(dir, "modules", producer.module)
		data := contract(t, filepath.Join(moduleDir, "services", producer.module, "proto"))
		path := "contracts/api/" + producer.module + "/grpc/contract.binpb"
		if err := os.MkdirAll(filepath.Join(moduleDir, filepath.Dir(filepath.FromSlash(path))), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(moduleDir, filepath.FromSlash(path)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		catalog := &composition.APIContractCatalog{
			Schema:  composition.APIContractCatalogSchema,
			Package: "fixture/" + producer.module,
			Version: "0.1.0",
			Endpoints: []composition.APIContractEndpoint{{
				Service:  producer.module,
				Endpoint: "grpc",
				API:      standards.GRPC,
				Kind:     composition.APIContractKindProtobuf,
				Package:  producer.pkg,
				Path:     path,
				Digest:   composition.APIContractDigest(data),
				Services: []composition.APIContractService{{
					Name:       producer.service,
					FullName:   producer.pkg + "." + producer.service,
					Procedures: []string{"/" + producer.pkg + "." + producer.service + "/Get"},
				}},
			}},
		}
		encoded, err := catalog.CanonicalBytes()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(moduleDir, "contracts", "api", "catalog.codefly.json"), encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// syntheticContract stands in for a descriptor set where nothing reaches
// codegen.
func syntheticContract(_ *testing.T, protoDir string) []byte {
	return []byte("synthetic-descriptor-set " + protoDir)
}

func backendServiceFile(wsDir string) string {
	return filepath.Join(wsDir, "services", "backend", resources.ServiceConfigurationName)
}

// Gap 1: the manifest names no executor. core's strict Load refuses it for
// that; this command reads only api.consumes and must not.
func TestSyncSolutionSDKReadsAManifestWithoutAnAgent(t *testing.T) {
	wsDir := newTwoModuleSolution(t, syntheticContract)
	data, err := os.ReadFile(filepath.Join(wsDir, manifest.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(data); err == nil || !strings.Contains(err.Error(), "agent.kind") {
		t.Fatalf("precondition: the fixture must be a manifest core's executor loader refuses for its agent block; got %v", err)
	}

	t.Chdir(wsDir)
	resetSolutionSDKFlags(t)
	if err := runSyncSolutionSDK(context.Background()); err != nil {
		t.Fatalf("a solution manifest without an agent block must sync: %v", err)
	}
}

// Gap 2: a consume of a module's connect or rest endpoint resolves to that
// service's protobuf contract, which the catalog carries under grpc.
func TestSyncSolutionSDKResolvesTransportEndpointsToTheProtobufContract(t *testing.T) {
	wsDir := newTwoModuleSolution(t, syntheticContract)
	ctx := context.Background()
	ws := loadFixtureWorkspace(t, wsDir)
	data, err := os.ReadFile(filepath.Join(wsDir, manifest.FileName))
	if err != nil {
		t.Fatal(err)
	}
	declared, err := manifest.LoadConsumption(data)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := resolveConsumedContracts(ctx, ws, declared)
	if err != nil {
		t.Fatalf("resolveConsumedContracts: %v", err)
	}
	if len(entries) != len(fixtureProducers) {
		t.Fatalf("resolved %d entries, want %d", len(entries), len(fixtureProducers))
	}
	for i, producer := range fixtureProducers {
		got := entries[i].Endpoint
		if got.Endpoint != "grpc" || got.Kind != composition.APIContractKindProtobuf || got.Package != producer.pkg {
			t.Errorf("consume of %s resolved to %s/%s %s %s, want the grpc protobuf contract %s", producer.module, got.Service, got.Endpoint, got.Kind, got.Package, producer.pkg)
		}
	}

	// A consume of the connect endpoint itself, once the catalog publishes it
	// beside grpc under the same contract — one destination behind the hop —
	// resolves to it; a rest consume resolves to that one contract too.
	// Different contracts on the two give the rest consume no single answer.
	catalog, err := composition.LoadAPIContractCatalog(filepath.Join(wsDir, "modules", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	connect := catalog.Endpoints[0]
	connect.Endpoint, connect.API = "connect", standards.CONNECT
	catalog.Endpoints = append(catalog.Endpoints, connect)
	accounts := declared.API.Consumes[0]
	if got, resolveErr := consumedContract(ctx, "", catalog, &accounts); resolveErr != nil || got.Endpoint != "connect" {
		t.Fatalf("connect consume resolved to %+v (%v), want the published connect contract", got, resolveErr)
	}
	accounts.Endpoint = "rest"
	catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{Service: "accounts", Endpoint: "rest", API: standards.REST, Kind: composition.APIContractKindOpenAPI})
	if got, resolveErr := consumedContract(ctx, "", catalog, &accounts); resolveErr != nil || got.Kind != composition.APIContractKindProtobuf {
		t.Fatalf("rest consume over one contract on grpc and connect resolved to %+v (%v), want that protobuf contract", got, resolveErr)
	}
	catalog.Endpoints[1].Digest = "sha256:another"
	if _, resolveErr := consumedContract(ctx, "", catalog, &accounts); resolveErr == nil || !strings.Contains(resolveErr.Error(), "different protobuf contracts") {
		t.Fatalf("error = %v, want two contracts on one service refused", resolveErr)
	}

	// An endpoint the producer does not declare is still refused, by name.
	declared.API.Consumes[1].Endpoint = "graphql"
	if _, err := resolveConsumedContracts(ctx, ws, declared); err == nil || !strings.Contains(err.Error(), "declares no endpoint graphql") {
		t.Fatalf("error = %v, want the undeclared endpoint named", err)
	}
}

// Gap 3: both consumes are called through the authority hop's Connect face,
// so neither the check nor --apply-dependencies asks for a service-dependency:
// that edge is a mesh path past the hop, a call no method policy is checked on.
func TestSyncSolutionSDKAsksNoMeshEdgeForAHopRoutedConsume(t *testing.T) {
	wsDir := newTwoModuleSolution(t, syntheticContract)
	before, err := os.ReadFile(backendServiceFile(wsDir))
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(wsDir)
	resetSolutionSDKFlags(t)
	solutionSDKApply = true
	if err := runSyncSolutionSDK(context.Background()); err != nil {
		t.Fatalf("runSyncSolutionSDK --apply-dependencies: %v", err)
	}
	after, err := os.ReadFile(backendServiceFile(wsDir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("--apply-dependencies added a mesh edge past the hop:\n%s", after)
	}

	// --check counts the absent library, and nothing else.
	resetSolutionSDKFlags(t)
	solutionSDKCheck = true
	if err := runSyncSolutionSDK(context.Background()); err == nil || !strings.Contains(err.Error(), "(1 problem(s))") {
		t.Fatalf("--check = %v, want only the absent library reported", err)
	}
}

// buildFixtureContract compiles a fixture proto directory with buf, as
// `generate contracts` would export it.
func buildFixtureContract(t *testing.T, protoDir string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "contract.binpb")
	cmd := exec.Command("buf", "build", protoDir, "-o", out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("buf build %s: %v\n%s", protoDir, err, output)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The whole issue, through the real proto companion: the solution SDK of a
// solution with no agent block, consuming two modules over connect and rest
// with no service-dependencies, generates a Go half that builds (the command
// compiles it) and a TypeScript half, and --check then passes on it.
func TestSyncSolutionSDKTwoModulesEndToEnd(t *testing.T) {
	conformancetest.Gate(t, "linux-amd64-docker-generate", "docker", "buf")
	wsDir := newTwoModuleSolution(t, buildFixtureContract)
	ctx := context.Background()

	t.Chdir(wsDir)
	resetSolutionSDKFlags(t)
	skipGenerateForTests = false
	solutionSDKLanguages = []string{"go", "typescript"}
	if err := runSyncSolutionSDK(ctx); err != nil {
		t.Fatalf("runSyncSolutionSDK: %v", err)
	}

	library := filepath.Join(wsDir, "libraries", "portal-sdk")
	t.Cleanup(func() {
		if t.Failed() {
			_ = filepath.WalkDir(library, func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					t.Log(strings.TrimPrefix(path, library+string(filepath.Separator)))
				}
				return nil
			})
		}
	})
	for _, path := range []string{
		"go/accounts/accounts_facade.pb.go",
		"go/records/records_facade.pb.go",
		"typescript/src/index.ts",
		"typescript/src/accounts_facade.ts",
		"typescript/src/records_facade.ts",
		"typescript/src/gen/accounts",
		"typescript/src/gen/records",
	} {
		if _, err := os.Stat(filepath.Join(library, filepath.FromSlash(path))); err != nil {
			t.Errorf("generated library lacks %s: %v", path, err)
		}
	}

	resetSolutionSDKFlags(t)
	skipGenerateForTests = false
	solutionSDKLanguages = []string{"go", "typescript"}
	solutionSDKCheck = true
	if err := runSyncSolutionSDK(ctx); err != nil {
		t.Fatalf("--check on the library just generated: %v", err)
	}
}
