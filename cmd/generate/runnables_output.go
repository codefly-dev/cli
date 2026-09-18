package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/cli/pkg/cli"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/shared"
	"github.com/pmezard/go-difflib/difflib"
)

func newOperationDocument(spec *corerunnable.OperationSpec) *runnablespkg.Operation {
	return &runnablespkg.Operation{
		Method:         spec.Method,
		AttemptTimeout: spec.AttemptTimeout.String(),
		TotalTimeout:   spec.TotalTimeout.String(),
		MaxAttempts:    spec.MaxAttempts,
		Backoff:        spec.Backoff.String(),
		RetryableCodes: spec.RetryableCodes,
		Audience:       spec.Audience,
		InvokeScopes:   scopeDocuments(spec.InvokeScopes),
		LookupScopes:   scopeDocuments(spec.LookupScopes),
		LookupMethod:   spec.LookupMethod,
	}
}

func scopeDocuments(scopes []*basev0.WorkScopeV1) []runnablespkg.Scope {
	documents := make([]runnablespkg.Scope, 0, len(scopes))
	for _, scope := range scopes {
		documents = append(documents, runnablespkg.Scope{
			ResourceKind: scope.GetResourceKind(),
			Actions:      scope.GetActions(),
			ResourceIDs:  scope.GetResourceIds(),
		})
	}
	return documents
}

// marshalIndented encodes a committed document. Struct field order is the key
// order, so the bytes are the same on every run and a diff of two generations
// shows only what changed.
func marshalIndented(document any) ([]byte, error) {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeDerivedTree replaces the contents of output with files. The tree is
// owned by this command, so a directory left by a method that no longer
// carries the option is removed rather than merged with: a stale package is a
// contract nobody publishes any more.
func writeDerivedTree(ctx context.Context, files map[string][]byte, output string) error {
	if _, err := shared.CheckDirectoryOrCreate(ctx, output); err != nil {
		return fmt.Errorf("cannot create output directory: %w", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = os.RemoveAll(filepath.Join(output, entry.Name())); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(files) {
		target := filepath.Join(output, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err = shared.WriteFileAtomic(ctx, target, files[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// runRunnablesCheck reports every way the committed tree differs from what
// this walk would write, as a unified diff. It is the CI gate: a contract
// whose methods changed without the derived packages being regenerated is a
// module publishing operations it no longer implements.
func runRunnablesCheck(derived *derivation, output string) error {
	existing, err := listRelativeFiles(output)
	if err != nil {
		return err
	}

	names := map[string]struct{}{}
	for name := range derived.files {
		names[name] = struct{}{}
	}
	for name := range existing {
		names[name] = struct{}{}
	}

	var diffs []string
	for _, name := range sortedKeys(names) {
		generated, wanted := derived.files[name]
		onDisk, present := existing[name]
		if wanted && present && string(generated) == string(onDisk) {
			continue
		}
		diff, diffErr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(string(onDisk)),
			B:        difflib.SplitLines(string(generated)),
			FromFile: "committed/" + name,
			ToFile:   "generated/" + name,
			Context:  3,
		})
		if diffErr != nil {
			return diffErr
		}
		diffs = append(diffs, diff)
	}

	if refusal := derived.refusal(); refusal != nil {
		return refusal
	}
	if len(diffs) == 0 {
		cli.Header(1, "Derived runnables are up to date")
		return nil
	}
	cli.Warning("Derived runnables are out of date:")
	for _, diff := range diffs {
		fmt.Print(diff)
	}
	return fmt.Errorf("derived runnables differ from %s (%d file(s)); run `codefly generate runnables` to update", output, len(diffs))
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
