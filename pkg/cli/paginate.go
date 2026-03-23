package cli

import (
	"fmt"

	"github.com/codefly-dev/core/tui"
)

func Paginate(s string) {
	if err := tui.RunPaginate(s); err != nil {
		fmt.Println("could not start program:", err)
	}
}
