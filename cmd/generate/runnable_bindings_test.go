package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/encoding/protojson"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func resetRunnableBindingsFlags(t *testing.T) {
	t.Helper()
	runnableBindingsEnv, runnableBindingsCheck = "local", false
	t.Cleanup(func() { runnableBindingsEnv, runnableBindingsCheck = "local", false })
}

// Every derived operation is bound to its owner endpoint at the address the
// environment resolves, verified against its package, with the policy the
// method declared and a reference, by digest, to its owner endpoint's
// descriptor set. The operations of one endpoint share one set, written once;
// --check holds the committed file to all of it.
func TestGenerateRunnableBindingsBindsEveryDerivedOperation(t *testing.T) {
	ctx := context.Background()
	second := markedMethod("ApplyTextAgain", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", conformingOperation())
	root, _ := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation(), second)), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "#") {
		t.Fatalf("want a header, two bindings and one shared descriptor set:\n%s", raw)
	}
	values := map[string]string{}
	var operations []string
	for _, line := range lines[1:] {
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
		if !strings.HasPrefix(key, corerunnable.DescriptorSetKeyPrefix) {
			operations = append(operations, key)
		}
	}
	lookup := func(key string) (string, error) {
		if value, ok := values[key]; ok {
			return value, nil
		}
		return "", fmt.Errorf("no value for %s", key)
	}
	if len(operations) != 2 {
		t.Fatalf("want two operations, got %v", operations)
	}
	var shared string
	for _, key := range operations {
		if !strings.HasPrefix(key, "DOCUMENTS__") {
			t.Fatalf("binding key %q is not scoped by its module", key)
		}
		prepared, err := corerunnable.DecodePrepared([]byte(values[key]))
		if err != nil {
			t.Fatal(err)
		}
		pkg := &basev0.RunnablePackage{}
		binding := &basev0.RunnableBinding{}
		if err := protojson.Unmarshal(prepared.Package, pkg); err != nil {
			t.Fatal(err)
		}
		if err := protojson.Unmarshal(prepared.Binding, binding); err != nil {
			t.Fatal(err)
		}
		if err := corerunnable.VerifyBinding(binding, pkg); err != nil {
			t.Fatal("binding does not verify against its package: ", err)
		}
		endpoint := binding.GetTarget().GetService().GetEndpoint()
		if endpoint.GetEndpoint().GetName() != "grpc" || len(endpoint.GetInstances()) != 1 || !strings.Contains(endpoint.GetInstances()[0].GetAddress(), "localhost:") {
			t.Fatalf("binding does not target the owner's resolved local address: %v", endpoint)
		}
		if binding.GetTarget().GetEnvironment() != "local" || !strings.HasPrefix(binding.GetTarget().GetRevision(), "sha256:") {
			t.Fatalf("binding target identity: %v", binding.GetTarget())
		}
		var policy struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(prepared.Operation, &policy); err != nil || policy.Method != pkg.GetServiceOperations()[0].GetOperation() {
			t.Fatalf("operation policy not carried: %s", prepared.Operation)
		}
		set, err := corerunnable.ResolveDescriptorSet(prepared.DescriptorSet, lookup)
		if err != nil {
			t.Fatal("owner descriptors do not resolve by digest: ", err)
		}
		descriptors := &descriptorpb.FileDescriptorSet{}
		if err := googleproto.Unmarshal(set, descriptors); err != nil {
			t.Fatal(err)
		}
		for _, file := range descriptors.GetFile() {
			if file.GetSourceCodeInfo() != nil {
				t.Fatalf("%s carries source info", file.GetName())
			}
		}
		files, err := protodesc.NewFiles(descriptors)
		if err != nil {
			t.Fatal(err)
		}
		method := strings.ReplaceAll(strings.TrimPrefix(policy.Method, "/"), "/", ".")
		if _, err := files.FindDescriptorByName(protoreflect.FullName(method)); err != nil {
			t.Fatal("the method does not resolve in its owner's descriptors: ", err)
		}
		if shared != "" && shared != prepared.DescriptorSet.Digest {
			t.Fatal("two operations on one endpoint reference different sets")
		}
		shared = prepared.DescriptorSet.Digest
	}

	// The committed file is current; a stale one is refused by --check.
	runnableBindingsCheck = true
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(raw, []byte("STALE=1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err == nil {
		t.Fatal("--check accepted a stale file")
	}
}

// A contract whose bytes are not the ones the catalog recorded is refused: a
// reference to it would name a contract nobody published.
func TestGenerateRunnableBindingsRefusesAStaleContractCatalog(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	catalog, err := composition.LoadAPIContractCatalog(moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	contract := filepath.Join(moduleDir, filepath.FromSlash(catalog.Endpoints[0].Path))
	changed := descriptorSet(t, ingestionFile(conformingOperation(), markedMethod("Extra", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil)))
	if err := os.WriteFile(contract, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	err = RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "codefly generate contracts") {
		t.Fatalf("a stale catalog was not refused: %v", err)
	}
}

func TestRunnableBindingKeyIsAnEnvironmentKey(t *testing.T) {
	if got := RunnableBindingKey("documents", "documents.ingest-document"); got != "DOCUMENTS__DOCUMENTS_INGEST_DOCUMENT" {
		t.Fatalf("key %q", got)
	}
}
