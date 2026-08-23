//go:build !codefly_nosemantic

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
// This is the default variant: it is selected for every build that does NOT set
// the `codefly_nosemantic` tag. The analyzer's tree-sitter bindings require cgo,
// so a build that disables cgo without also setting the tag selects this file
// and fails to link ("build constraints exclude all Go files") — a loud signal
// that a normal CLI needs cgo, rather than a silent drop of the gateway. Only
// the in-container static builds that opt in with `codefly_nosemantic` get the
// analyzer-free variant — see source_nosemantic.go.
func newSource(root string) *Source {
	return &Source{server: codecore.NewDefaultCodeServer(root, codecore.WithSemanticAnalyzer(semantic.New()))}
}
