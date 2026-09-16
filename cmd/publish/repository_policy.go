package publish

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/librarystore"
)

// repositoryPolicyFrom builds the repository-creation policy a publish run
// carries, from the two flags that express it.
//
// It is a pure function of its arguments rather than a reader of the command's
// flag globals, and it is shared by `publish library` and `publish clients`
// rather than each assembling the policy inline. Both properties are deliberate.
// The policy used to travel inside librarystore.StoreConfig, which every publish
// path re-loads from the workspace file — and `publish clients` re-loads it a
// second time inside its lock, which silently dropped the flags on the one path
// that actually publishes. A policy that is built once from arguments and passed
// down cannot be lost by a reload, and cannot be spelled in a file that travels
// with a module.
func repositoryPolicyFrom(createMissing, public bool) librarystore.RepositoryPolicy {
	return librarystore.RepositoryPolicy{CreateMissing: createMissing, Public: public}
}

// reportPublishWarnings prints the operator-facing notes a publish returned.
// These are not failures — the versions are live — so they follow the result
// table rather than replacing it. The store reports them as data so that the
// presentation lives here and a test can assert them without capturing output.
func reportPublishWarnings(published []publishedExport) {
	for _, p := range published {
		for _, warning := range p.Warnings {
			cli.Warning("%s: %s", p.Language, warning)
		}
	}
}
