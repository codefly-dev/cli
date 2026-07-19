package cli

import (
	"fmt"
	"os"

	"github.com/codefly-dev/core/tui"
	"golang.org/x/term"
)

// Paginate shows long output interactively when a terminal is available.
// Default/headless commands print directly: a non-interactive command must
// never attempt to open /dev/tty merely because generated output is long.
func Paginate(s string) {
	if !shouldPaginate(WithDefault(), term.IsTerminal(int(os.Stdin.Fd())), term.IsTerminal(int(os.Stdout.Fd()))) {
		fmt.Println(s)
		return
	}
	if err := tui.RunPaginate(s); err != nil {
		fmt.Println("could not start program:", err)
	}
}

func shouldPaginate(withDefault, stdinTerminal, stdoutTerminal bool) bool {
	return !withDefault && stdinTerminal && stdoutTerminal
}
