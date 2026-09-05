package cmd

import "testing"

func TestServerCommandHasOpenFlag(t *testing.T) {
	if ServerCmd.Flags().Lookup("open") == nil {
		t.Fatal("codefly server has no --open flag")
	}
}
