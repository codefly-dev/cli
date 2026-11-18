// Print the same published image reference that generate uses. CI must not
// duplicate Core's registry or version pin when pre-pulling its companion.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/core/companions/proto"

	// Registers the companion index this CLI carries: core compiles no image version in.
	_ "github.com/codefly-dev/cli/pkg/companions"
)

func main() {
	image, err := proto.CompanionImage(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(image.FullName())
}
