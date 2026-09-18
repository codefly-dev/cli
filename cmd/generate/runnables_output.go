package generate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// generatedFileNames are the only file names a generation produces. Clearing
// is scoped to them because --output names a directory this command does not
// necessarily own alone: "--output=contracts", one word short of
// "contracts/runnables", would otherwise delete the module's whole published
// api contracts tree. `generate contracts` removes only the per-endpoint
// directories it writes and never the root's contents; this matches that.
var generatedFileNames = map[string]bool{
	runnablespkg.IndexFileName:     true,
	runnablespkg.PackageFileName:   true,
	runnablespkg.OperationFileName: true,
}

// writeDerivedTree makes output hold exactly files. A directory left by a
// method that no longer carries the option is removed rather than merged with
// — a stale package is a contract nobody publishes any more — but only the
// files a generation produces are removed, and only the directories those
// files emptied.
func writeDerivedTree(ctx context.Context, files map[string][]byte, output string) error {
	for _, name := range sortedKeys(files) {
		if err := runnablespkg.ValidateRelativePath("derived runnable path", name); err != nil {
			return err
		}
	}
	if _, err := shared.CheckDirectoryOrCreate(ctx, output); err != nil {
		return fmt.Errorf("cannot create output directory: %w", err)
	}
	if err := clearGeneratedFiles(output); err != nil {
		return err
	}
	for _, name := range sortedKeys(files) {
		target := filepath.Join(output, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := shared.WriteFileAtomic(ctx, target, files[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// clearGeneratedFiles removes every file a previous generation wrote under
// output, then prunes the directories that leaves empty, deepest first. A
// directory still holding anything this command did not write is kept, along
// with whatever is in it.
//
// Every removal resolves through an os.Root anchored at output. Walking a tree
// and then deleting by the walked path is resolved twice, and a path component
// swapped for a symlink in between would put a delete outside the directory
// the operator named; the root makes the second resolution impossible to
// redirect. Symlinks are never descended, so a link inside the tree is at most
// removed as the link it is.
func clearGeneratedFiles(output string) error {
	root, err := os.OpenRoot(output)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var directories []string
	err = fs.WalkDir(root.FS(), ".", func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if p != "." {
				directories = append(directories, p)
			}
			return nil
		}
		if generatedFileNames[path.Base(p)] {
			return root.Remove(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(directories)))
	for _, directory := range directories {
		entries, readErr := fs.ReadDir(root.FS(), directory)
		if readErr != nil {
			return readErr
		}
		if len(entries) > 0 {
			continue
		}
		if err = root.Remove(directory); err != nil {
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

	refusal := derived.refusal()
	if len(diffs) == 0 {
		if refusal != nil {
			return refusal
		}
		cli.Header(1, "Derived runnables are up to date")
		return nil
	}
	cli.Warning("Derived runnables are out of date:")
	for _, diff := range diffs {
		fmt.Print(diff)
	}
	// Both problems are reported from one run. A refusal returned on its own
	// would discard the diff already computed here, and send whoever fixes the
	// contract back for a second run to discover the drift underneath it.
	drift := fmt.Errorf("derived runnables differ from %s (%d file(s)); run `codefly generate runnables` to update", output, len(diffs))
	if refusal != nil {
		return errors.Join(refusal, drift)
	}
	return drift
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
