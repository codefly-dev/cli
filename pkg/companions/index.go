// Package companions registers the companion index this CLI carries, so every
// core call that names a companion image (CompanionImage, BuildSpecs, Images)
// reads the CLI's choice. The shared command package imports it for its side
// effect, so both executable entry points register the same selection.
package companions

import (
	_ "embed"
	"fmt"

	companionindex "github.com/codefly-dev/core/companions/index"
)

//go:embed index.codefly.yaml
var indexDocument []byte

func init() {
	index, err := companionindex.Parse(indexDocument)
	if err != nil {
		panic(fmt.Sprintf("the CLI's companion index does not parse: %v", err))
	}
	if err := companionindex.Register(index); err != nil {
		panic(fmt.Sprintf("register the CLI's companion index: %v", err))
	}
}
