// Package runnablestest lays out the tree `codefly generate runnables` writes,
// for the surfaces that read it. Generation is tested against real descriptors
// in cmd/generate; list, show and the MCP tool are tested against this, so
// they exercise the same on-disk shape without each rebuilding a descriptor
// set to get at it.
package runnablestest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Method is the method the fixture's operation adapts.
const Method = "/documents.ingest.v1.IngestionService/ApplyText"

// Package builds the package one derived operation carries: a SERVICE package
// whose implementation is a method an owner service already publishes.
func Package(t *testing.T, workspace, module, name string) *basev0.RunnablePackage {
	t.Helper()
	pkg, err := corerunnable.PreparePackage(&basev0.RunnablePackage{
		Schema:   corerunnable.PackageSchemaV1,
		Identity: &basev0.RunnableIdentity{Workspace: workspace, Module: module, Name: name, Version: "0.1.0"},
		Agent:    &basev0.Agent{Kind: basev0.Agent_SERVICE, Name: "go", Version: "0.0.48", Publisher: "codefly.dev"},
		Contract: &basev0.RunnableContract{
			Protocol: resources.RunnableServiceProtocolV1,
			Input:    &basev0.RunnableSchema{Fields: []*basev0.RunnableField{{Name: "text", Type: basev0.RunnableField_STRING}}},
			Output:   &basev0.RunnableSchema{Fields: []*basev0.RunnableField{{Name: "minted", Type: basev0.RunnableField_BOOLEAN}}},
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
			Module: module, Name: "runtime-worker", Endpoint: "grpc",
			Operation:     Method,
			InputMessage:  "documents.ingest.v1.ApplyTextRequest",
			OutputMessage: "documents.ingest.v1.ApplyTextResponse",
			Adaptation:    basev0.RunnableServiceOperation_ADAPTATION_BOUNDED_JSON_V1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

// Write lays out one derived operation under moduleDir and returns its
// package, so a caller can assert against the digest it was written with.
func Write(t *testing.T, moduleDir, workspace, module, name string) *basev0.RunnablePackage {
	t.Helper()
	pkg := Package(t, workspace, module, name)
	relative := filepath.Join("contracts", "runnables", "runtime-worker", "grpc", "ApplyText")

	packageBytes, err := runnables.CanonicalJSON(pkg)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(moduleDir, relative, runnables.PackageFileName), packageBytes)
	write(t, filepath.Join(moduleDir, relative, runnables.OperationFileName), marshal(t, &runnables.Operation{
		Method:         Method,
		AttemptTimeout: "10s",
		TotalTimeout:   "1m0s",
		MaxAttempts:    1,
		Backoff:        "1s",
		RetryableCodes: []string{"UNAVAILABLE"},
		Audience:       "documents.ingestion",
		InvokeScopes:   []runnables.Scope{{ResourceKind: "documents", Actions: []string{"ingest", "read"}}},
		LookupScopes:   []runnables.Scope{{ResourceKind: "documents", Actions: []string{"read"}}},
		LookupMethod:   "/documents.ingest.v1.IngestionService/LookupText",
	}))
	write(t, filepath.Join(moduleDir, "contracts", "runnables", runnables.IndexFileName), marshal(t, &runnables.Index{
		Schema:    runnables.IndexSchema,
		Workspace: workspace,
		Module:    module,
		Operations: []runnables.IndexEntry{{
			Name:          name,
			Version:       "0.1.0",
			Digest:        pkg.GetDigest(),
			Service:       "runtime-worker",
			Endpoint:      "grpc",
			Method:        Method,
			InputMessage:  "documents.ingest.v1.ApplyTextRequest",
			OutputMessage: "documents.ingest.v1.ApplyTextResponse",
			Path:          filepath.ToSlash(relative),
		}},
	}))
	return pkg
}

func marshal(t *testing.T, document any) []byte {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
