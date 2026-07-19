package cli

import "testing"

func TestShouldPaginateRequiresInteractiveInputAndOutput(t *testing.T) {
	tests := []struct {
		name           string
		withDefault    bool
		stdinTerminal  bool
		stdoutTerminal bool
		want           bool
	}{
		{name: "interactive", stdinTerminal: true, stdoutTerminal: true, want: true},
		{name: "default mode", withDefault: true, stdinTerminal: true, stdoutTerminal: true},
		{name: "headless input", stdoutTerminal: true},
		{name: "redirected output", stdinTerminal: true},
		{name: "fully headless"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := shouldPaginate(test.withDefault, test.stdinTerminal, test.stdoutTerminal)
			if got != test.want {
				t.Fatalf("shouldPaginate(%t, %t, %t) = %t, want %t",
					test.withDefault, test.stdinTerminal, test.stdoutTerminal, got, test.want)
			}
		})
	}
}
