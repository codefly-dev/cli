//go:build cgo

package engine

import (
	codecore "github.com/codefly-dev/core/code"
	"github.com/codefly-dev/core/code/semantic"
)

// Core omits the tree-sitter analyzer by default so Go service agents stay
// CGO-free. The CLI is the workspace-wide source behavior behind the gateway —
// semantic index and symbol mutation are part of its contract — so it installs
// the analyzer explicitly. See core/code.WithSemanticAnalyzer.
//
// The analyzer's tree-sitter bindings require cgo, so this variant is only
// compiled for cgo builds. The !cgo variant (used by the statically linked
// companion CLI) drops it — see source_nocgo.go.
func newSource(root string) *Source {
	return &Source{server: codecore.NewDefaultCodeServer(root, codecore.WithSemanticAnalyzer(semantic.New()))}
}
