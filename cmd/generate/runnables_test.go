package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/encoding/protojson"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func resetRunnablesFlags(t *testing.T) {
	t.Helper()
	prevOutput, prevCheck := runnablesOutput, runnablesCheck
	runnablesOutput, runnablesCheck = "", false
	t.Cleanup(func() { runnablesOutput, runnablesCheck = prevOutput, prevCheck })
}

func generatedTree(t *testing.T, moduleDir string) []string {
	t.Helper()
	root := filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir))
	var paths []string
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

func readIndex(t *testing.T, moduleDir string) *runnablespkg.Index {
	t.Helper()
	index, err := runnablespkg.LoadIndex(moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	if index == nil {
		t.Fatal("no index was written")
	}
	return index
}

// TestGenerateRunnablesDerivesMarkedMethodsAndRefusesTheRest is the golden
// case: one conforming method, one marked method outside the bounded profile
// and one marked method that streams. The conforming one is derived, the other
// two are named, and the command still fails.
func TestGenerateRunnablesDerivesMarkedMethodsAndRefusesTheRest(t *testing.T) {
	ctx := context.Background()
	contract := descriptorSet(t, ingestionFile(conformingOperation(), outOfProfileMethod(), streamingMethod()))
	root, moduleDir := saveRunnableFixture(t, ctx, contract, "0.1.0")

	t.Chdir(root)
	resetRunnablesFlags(t)

	err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
	if err == nil {
		t.Fatal("expected the refused methods to fail the command")
	}
	for _, want := range []string{
		"/documents.ingest.v1.IngestionService/CountText",
		"documents.ingest.v1.CountRequest.total",
		"/documents.ingest.v1.IngestionService/StreamText",
		"streams",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal = %v, want it to name %q", err, want)
		}
	}

	// A command that fails writes nothing at all: see
	// TestGenerateRunnablesRefusalLeavesTheCommittedTreeAlone for why the
	// conforming methods do not get written either.
	derivedDir := filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir))
	if _, statErr := os.Stat(derivedDir); !os.IsNotExist(statErr) {
		t.Fatalf("the refused run wrote %s (stat err %v)", derivedDir, statErr)
	}

	// Removing the two offending methods derives the rest, which is what the
	// tree is expected to hold.
	publishContract(t, moduleDir, descriptorSet(t, ingestionFile(conformingOperation())))
	if err = RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"index.json",
		"runtime-worker/grpc/ApplyText/operation.json",
		"runtime-worker/grpc/ApplyText/runnable-package.json",
	}
	if got := generatedTree(t, moduleDir); !equalStrings(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}

	index := readIndex(t, moduleDir)
	if len(index.Operations) != 1 {
		t.Fatalf("index has %d operations, want 1", len(index.Operations))
	}
	entry := index.Operations[0]
	if entry.Name != "runtime-worker-grpc-apply-text" {
		t.Fatalf("derived name = %q", entry.Name)
	}
	if entry.Version != "0.1.0" {
		t.Fatalf("derived version = %q, want the module's release version", entry.Version)
	}
	if entry.Method != applyTextMethod {
		t.Fatalf("method = %q", entry.Method)
	}
	if entry.InputMessage != "documents.ingest.v1.ApplyTextRequest" || entry.OutputMessage != "documents.ingest.v1.ApplyTextResponse" {
		t.Fatalf("messages = %s -> %s", entry.InputMessage, entry.OutputMessage)
	}
	if entry.Path != "contracts/runnables/runtime-worker/grpc/ApplyText" {
		t.Fatalf("path = %q", entry.Path)
	}
	if entry.Source() != "runtime-worker/grpc/ApplyText" {
		t.Fatalf("source = %q", entry.Source())
	}

	// The package on disk is the canonical form its own digest was taken
	// over, so what a composition reads and what core hashed are the same
	// bytes.
	pkg := readDerivedPackage(t, moduleDir, entry)
	if err = corerunnable.VerifyPackage(pkg); err != nil {
		t.Fatalf("the package written to disk does not verify: %v", err)
	}
	if pkg.GetDigest() != entry.Digest {
		t.Fatalf("index digest %s does not match the package's %s", entry.Digest, pkg.GetDigest())
	}
	if len(pkg.GetExecution().GetFacilities()) != 1 || pkg.GetExecution().GetFacilities()[0].GetKind() != basev0.RunnableFacility_SERVICE {
		t.Fatalf("facilities = %v, want one SERVICE", pkg.GetExecution().GetFacilities())
	}

	operation := readDerivedOperation(t, moduleDir, entry)
	if operation.Method != applyTextMethod {
		t.Fatalf("operation method = %q", operation.Method)
	}
	if operation.Audience != "documents.ingestion" {
		t.Fatalf("audience = %q", operation.Audience)
	}
	if operation.LookupMethod != "/documents.ingest.v1.IngestionService/LookupText" {
		t.Fatalf("lookup method = %q", operation.LookupMethod)
	}
	// Policy and authority stay beside the package, never inside it: two
	// installations of one contract may run under different ones, and a
	// package that carried them would make those two installations two
	// releases.
	packageBytes := mustRead(t, filepath.Join(moduleDir, filepath.FromSlash(entry.Path), runnablespkg.PackageFileName))
	if strings.Contains(string(packageBytes), "documents.ingestion") {
		t.Fatal("the package carries the operation's audience; policy belongs beside it")
	}
}

// TestGenerateRunnablesIsIdempotent proves the output is a function of the
// contract: a second run changes nothing, and un-marking the method removes
// the directory rather than leaving a package nobody publishes.
func TestGenerateRunnablesIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")

	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	first := snapshotTree(t, moduleDir)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	if second := snapshotTree(t, moduleDir); !equalTrees(first, second) {
		t.Fatal("a second run changed the tree")
	}

	runnablesCheck = true
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatalf("--check reports drift against the tree it just wrote: %v", err)
	}
	runnablesCheck = false

	// Remove the option and the derived directory has to go with it.
	publishContract(t, moduleDir, descriptorSet(t, ingestionFile(nil)))
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	if got := generatedTree(t, moduleDir); !equalStrings(got, []string{"index.json"}) {
		t.Fatalf("tree = %v, want the stale directory gone and an empty index", got)
	}
	if operations := readIndex(t, moduleDir).Operations; len(operations) != 0 {
		t.Fatalf("index has %d operations, want none", len(operations))
	}
}

// TestGenerateRunnablesCheckReportsDriftAsAUnifiedDiff is the CI gate: a
// committed package that no longer matches its contract fails with the diff
// that says what moved.
func TestGenerateRunnablesCheckReportsDriftAsAUnifiedDiff(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")

	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}

	operationFile := filepath.Join(moduleDir, filepath.FromSlash("contracts/runnables/runtime-worker/grpc/ApplyText/operation.json"))
	edited := strings.Replace(string(mustRead(t, operationFile)), `"max_attempts": 1`, `"max_attempts": 4`, 1)
	writeFixtureFile(t, operationFile, []byte(edited))

	runnablesCheck = true
	output, err := captureStdout(t, func() error { return RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}) })
	if err == nil {
		t.Fatal("expected --check to fail on drift")
	}
	if !strings.Contains(err.Error(), "run `codefly generate runnables` to update") {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{
		"--- committed/runtime-worker/grpc/ApplyText/operation.json",
		"+++ generated/runtime-worker/grpc/ApplyText/operation.json",
		`-  "max_attempts": 4`,
		`+  "max_attempts": 1`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("diff does not contain %q:\n%s", want, output)
		}
	}

	// --check writes nothing, so the edited file is still the edited file.
	if !strings.Contains(string(mustRead(t, operationFile)), `"max_attempts": 4`) {
		t.Fatal("--check wrote to the tree")
	}
}

// TestGenerateRunnablesWithoutAMarkedMethodWritesAnEmptyIndex: a module that
// derives nothing is not an error. Its contracts are still published, and an
// empty index is the answer a composition needs.
func TestGenerateRunnablesWithoutAMarkedMethodWritesAnEmptyIndex(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(nil)), "0.1.0")

	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	index := readIndex(t, moduleDir)
	if len(index.Operations) != 0 {
		t.Fatalf("index has %d operations, want none", len(index.Operations))
	}
	if index.Module != "documents" || index.Workspace != "test-workspace" {
		t.Fatalf("index = %+v", index)
	}
}

// TestGenerateRunnablesFallsBackToAContractDigestIdentity: a module with no
// package manifest has no release version to carry, so the contract's own
// digest stands in — a valid semantic version that moves when the contract
// does.
func TestGenerateRunnablesFallsBackToAContractDigestIdentity(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "")

	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	version := readIndex(t, moduleDir).Operations[0].Version
	if !strings.HasPrefix(version, "0.0.0-contract-") || len(version) != len("0.0.0-contract-")+12 {
		t.Fatalf("version = %q, want 0.0.0-contract-<digest12>", version)
	}
	if _, err := semver.StrictNewVersion(version); err != nil {
		t.Fatalf("derived version %q is not a strict semantic version: %v", version, err)
	}
}

// A prerelease identifier made only of digits may not carry a leading zero, so
// a bare digest prefix spells an invalid version for roughly one contract in
// 1400 — and every method in that module would then be refused, with a message
// naming the method rather than the version. The identifier has to stay
// alphanumeric whatever the digest is.
func TestDerivedVersionIsValidSemverForEveryDigest(t *testing.T) {
	for _, digest := range []string{
		"012345678901", // all digits, leading zero: invalid on its own
		"000000000000",
		"123456789012",
		"4c26fb95a023",
		"abcdefabcdef",
	} {
		version := derivedVersion("", &composition.APIContractEndpoint{Digest: "sha256:" + digest + strings.Repeat("0", 52)})
		if _, err := semver.StrictNewVersion(version); err != nil {
			t.Errorf("digest %s derives %q, which is not a strict semantic version: %v", digest, version, err)
		}
	}
	if got := derivedVersion("1.4.2", &composition.APIContractEndpoint{Digest: "sha256:012345678901"}); got != "1.4.2" {
		t.Fatalf("a module with a release version derived %q instead of it", got)
	}
}

// A failing command must leave the tree exactly as it found it. Writing the
// methods that did derive deletes the refused one's committed package, which
// asserts the module no longer publishes an operation whose payload the owner
// merely broke — and the deletion outlives the non-zero exit.
func TestGenerateRunnablesRefusalLeavesTheCommittedTreeAlone(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	committed := snapshotTree(t, moduleDir)
	if len(committed) != 3 {
		t.Fatalf("expected the generated tree to hold 3 files, got %d", len(committed))
	}

	// The owner breaks ApplyText's payload with a uint64, outside the bounded
	// profile. The method still carries the option.
	broken := ingestionFile(conformingOperation())
	broken.MessageType[0].Field = append(broken.MessageType[0].Field, uint64Field("total", 99))
	publishContract(t, moduleDir, descriptorSet(t, broken))

	err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
	if err == nil {
		t.Fatal("expected the refusal to fail the command")
	}
	if after := snapshotTree(t, moduleDir); !equalTrees(committed, after) {
		t.Fatalf("the failed command mutated the committed tree: %d files before, %d after", len(committed), len(after))
	}
}

// --check computes the diff before it knows about a refusal; returning the
// refusal alone would throw that diff away and send whoever fixes the contract
// back for a second run to discover the drift underneath it.
func TestGenerateRunnablesCheckReportsBothARefusalAndDrift(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	operationFile := filepath.Join(moduleDir, filepath.FromSlash("contracts/runnables/runtime-worker/grpc/ApplyText/operation.json"))
	writeFixtureFile(t, operationFile, []byte(strings.Replace(string(mustRead(t, operationFile)), `"max_attempts": 1`, `"max_attempts": 4`, 1)))

	// A second method is marked and out of profile, so the run both refuses
	// and drifts.
	withRefusal := ingestionFile(conformingOperation(), outOfProfileMethod())
	publishContract(t, moduleDir, descriptorSet(t, withRefusal))

	runnablesCheck = true
	output, err := captureStdout(t, func() error { return RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}) })
	if err == nil {
		t.Fatal("expected --check to fail")
	}
	if !strings.Contains(err.Error(), "CountText") {
		t.Fatalf("error does not report the refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "run `codefly generate runnables` to update") {
		t.Fatalf("error does not report the drift: %v", err)
	}
	if !strings.Contains(output, `-  "max_attempts": 4`) {
		t.Fatalf("the diff was discarded:\n%s", output)
	}
}

// --output names a directory this command does not necessarily own alone.
// "--output=contracts", one word short of "contracts/runnables", must not
// delete the module's published api contracts.
func TestGenerateRunnablesOutputRemovesOnlyWhatItWrote(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)

	contracts := filepath.Join(moduleDir, "contracts")
	runnablesOutput = contracts
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}

	for _, kept := range []string{
		"api/runtime-worker/grpc/contract.binpb",
		"api/catalog.codefly.json",
	} {
		if _, err := os.Stat(filepath.Join(contracts, filepath.FromSlash(kept))); err != nil {
			t.Errorf("generating into %s destroyed %s: %v", contracts, kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(contracts, "index.json")); err != nil {
		t.Errorf("the index was not written into the chosen output: %v", err)
	}
}

// A module that declares no interface exports no endpoint, so it has no method
// to derive from: zero operations, which is an empty index rather than the
// "run generate contracts first" error a module that DOES export endpoints
// gets.
func TestGenerateRunnablesWithoutAnInterfaceWritesAnEmptyIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "test-workspace",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "documents"}},
	}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Join(root, "modules", "documents")
	module := &resources.Module{Kind: resources.ModuleKind, Name: "documents"}
	module.WithDir(moduleDir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatalf("a module exporting nothing should derive nothing, not fail: %v", err)
	}
	if operations := readIndex(t, moduleDir).Operations; len(operations) != 0 {
		t.Fatalf("index has %d operations, want none", len(operations))
	}
}

// TestGenerateRunnablesRefusesAnUnpinnedOwnerAgent: a derived package is an
// immutable release, and "latest" is not one. core refuses it deep inside
// package validation, so the CLI names the service that has to be edited.
func TestGenerateRunnablesRefusesAnUnpinnedOwnerAgent(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")

	service, err := resources.LoadServiceFromDir(ctx, filepath.Join(moduleDir, "services", "runtime-worker"))
	if err != nil {
		t.Fatal(err)
	}
	service.Agent.Version = ""
	if err = service.Save(ctx); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	resetRunnablesFlags(t)

	err = RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
	if err == nil {
		t.Fatal("expected an unpinned owner agent to fail")
	}
	if !strings.Contains(err.Error(), "documents/runtime-worker") || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("error = %v, want it to name the service and why", err)
	}
}

// TestGenerateRunnablesReproducesTheDocumentStoreOracle is the proof this is a
// lift rather than a rewrite. module-document-store already publishes this
// operation through a package its worker assembles by hand
// (ingestrunnable.Package, printed by the worker's --describe-runnable), and
// the package this command writes has to be those same bytes.
//
// The release identity is the one thing the command derives rather than reads:
// the owner named its runnable "ingest-text", and no rule over
// IngestionService.ApplyText produces that name. So the comparison substitutes
// the oracle's identity into the derived package and re-prepares it — every
// other field, and therefore the digest, is the derivation's own.
func TestGenerateRunnablesReproducesTheDocumentStoreOracle(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")

	t.Chdir(root)
	resetRunnablesFlags(t)

	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	derived := readDerivedPackage(t, moduleDir, readIndex(t, moduleDir).Operations[0])

	handWritten, err := corerunnable.PreparePackage(&basev0.RunnablePackage{
		Schema:   corerunnable.PackageSchemaV1,
		Identity: &basev0.RunnableIdentity{Workspace: "module-document-store", Module: "documents", Name: "ingest-text", Version: "0.1.0"},
		Agent:    &basev0.Agent{Kind: basev0.Agent_SERVICE, Name: "go", Version: "0.0.48", Publisher: "codefly.dev"},
		Contract: &basev0.RunnableContract{
			Protocol: resources.RunnableServiceProtocolV1,
			Input: &basev0.RunnableSchema{Fields: []*basev0.RunnableField{
				{Name: "origin", Type: basev0.RunnableField_STRING},
				{Name: "container", Type: basev0.RunnableField_STRING},
				{Name: "ref", Type: basev0.RunnableField_STRING},
				{Name: "path", Type: basev0.RunnableField_STRING},
				{Name: "revision", Type: basev0.RunnableField_STRING},
				{Name: "commit", Type: basev0.RunnableField_STRING},
				{Name: "cursor", Type: basev0.RunnableField_INTEGER},
				{Name: "content_type", Type: basev0.RunnableField_STRING},
				{Name: "text", Type: basev0.RunnableField_STRING},
			}},
			Output: &basev0.RunnableSchema{Fields: []*basev0.RunnableField{
				{Name: "reference", Type: basev0.RunnableField_STRING},
				{Name: "content_hash", Type: basev0.RunnableField_STRING},
				{Name: "effect_digest", Type: basev0.RunnableField_STRING},
				{Name: "minted", Type: basev0.RunnableField_BOOLEAN},
				{Name: "skipped", Type: basev0.RunnableField_BOOLEAN},
				{Name: "quarantined", Type: basev0.RunnableField_BOOLEAN},
			}},
		},
		Execution: &basev0.RunnableExecution{
			Facilities:     []*basev0.RunnableFacility{{Kind: basev0.RunnableFacility_SERVICE}},
			Timeout:        durationpb.New(time.Minute),
			Cancellation:   basev0.RunnableExecution_CANCELLATION_NONE,
			Recovery:       basev0.RunnableExecution_RECOVERY_RECEIPT,
			MaxInputBytes:  resources.DefaultRunnablePayloadBytes,
			MaxOutputBytes: resources.DefaultRunnablePayloadBytes,
		},
		ServiceOperations: []*basev0.RunnableServiceOperation{{
			Module: "documents", Name: "runtime-worker", Endpoint: "grpc",
			Operation:     applyTextMethod,
			InputMessage:  "documents.ingest.v1.ApplyTextRequest",
			OutputMessage: "documents.ingest.v1.ApplyTextResponse",
			Adaptation:    basev0.RunnableServiceOperation_ADAPTATION_BOUNDED_JSON_V1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	asOracle := googleproto.CloneOf(derived)
	asOracle.Identity = handWritten.GetIdentity()
	asOracle.Digest = ""
	asOracle, err = corerunnable.PreparePackage(asOracle)
	if err != nil {
		t.Fatal(err)
	}
	if !googleproto.Equal(handWritten, asOracle) {
		t.Fatalf("the derived package is not the one document-store publishes:\n%v", asOracle)
	}
	if asOracle.GetDigest() != "4c26fb95a0236b94107b30f34783cd893f2be36e0d157340c247013fd93a9a1f" {
		t.Fatalf("digest = %s, want the published oracle's", asOracle.GetDigest())
	}
}

func readDerivedPackage(t *testing.T, moduleDir string, entry runnablespkg.IndexEntry) *basev0.RunnablePackage {
	t.Helper()
	pkg := &basev0.RunnablePackage{}
	data := mustRead(t, filepath.Join(moduleDir, filepath.FromSlash(entry.Path), runnablespkg.PackageFileName))
	if err := protojson.Unmarshal(data, pkg); err != nil {
		t.Fatal(err)
	}
	return pkg
}

func readDerivedOperation(t *testing.T, moduleDir string, entry runnablespkg.IndexEntry) *runnablespkg.Operation {
	t.Helper()
	operation := &runnablespkg.Operation{}
	data := mustRead(t, filepath.Join(moduleDir, filepath.FromSlash(entry.Path), runnablespkg.OperationFileName))
	if err := json.Unmarshal(data, operation); err != nil {
		t.Fatal(err)
	}
	return operation
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapshotTree(t *testing.T, moduleDir string) map[string][]byte {
	t.Helper()
	root := filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir))
	files, err := listRelativeFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func equalTrees(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, content := range a {
		if string(b[name]) != string(content) {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The derived name is an identity, so it has to be stable and it has to
// separate two endpoints publishing one method — otherwise those two
// operations share one identity and carry two digests.
func TestDerivedNameSeparatesEndpointsAndSpellsAcronymsAsWords(t *testing.T) {
	for _, testCase := range []struct {
		service, endpoint, method, want string
	}{
		{"runtime-worker", "grpc", "ApplyText", "runtime-worker-grpc-apply-text"},
		{"runtime-worker", "connect", "ApplyText", "runtime-worker-connect-apply-text"},
		{"ingestion", "grpc", "ApplyHTTPText", "ingestion-grpc-apply-http-text"},
		{"ingestion", "grpc", "Apply", "ingestion-grpc-apply"},
		{"ingestion", "grpc", "ApplyV2", "ingestion-grpc-apply-v2"},
	} {
		if got := derivedName(testCase.service, testCase.endpoint, testCase.method); got != testCase.want {
			t.Errorf("derivedName(%s, %s, %s) = %q, want %q", testCase.service, testCase.endpoint, testCase.method, got, testCase.want)
		}
	}
}
