//go:build !cgo

package engine

import codecore "github.com/codefly-dev/core/code"

// Without cgo the tree-sitter semantic analyzer cannot be linked, so the source
// behavior runs with core's CGO-free default. This is the build the companion
// publisher produces (CGO_ENABLED=0, statically linked for alpine): the
// companion CLI only builds and runs services in-container and never serves the
// semantic gateway, so the analyzer is not needed. The cgo build installs it —
// see source_cgo.go.
func newSource(root string) *Source {
	return &Source{server: codecore.NewDefaultCodeServer(root)}
}
