//go:build codefly_nosemantic

package engine

import codecore "github.com/codefly-dev/core/code"

// Without the tree-sitter semantic analyzer the source behavior runs on core's
// CGO-free default. This variant is selected only when a build explicitly sets
// the `codefly_nosemantic` tag; the tag is the intent, and cgo being disabled is
// merely how these builds also happen to be linked. Gating on an explicit tag
// (rather than on `!cgo`) means an accidental CGO_ENABLED=0 build still fails
// loudly instead of silently producing an analyzer-less CLI — see
// source_semantic.go.
//
// Two build paths opt in, both producing statically linked binaries for alpine
// (CGO_ENABLED=0, -extldflags "-static") that only build and run services
// in-container and never serve the semantic gateway, so the analyzer is not
// needed: `codefly companion build`/`publish` (cmd/companion/build.go) and
// `codefly self build --os/--arch` (cmd/self/build.go, buildCLICross). Neither
// installs over the user's running CLI, which keeps cgo and the analyzer.
func newSource(root string) *Source {
	return &Source{server: codecore.NewDefaultCodeServer(root)}
}
