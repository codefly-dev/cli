package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestDeliveryKeyCommandIsRegisteredAndExplained(t *testing.T) {
	command, args, err := RootCmd.Find([]string{"delivery", "key"})
	if err != nil || len(args) != 0 || command == nil || command.CommandPath() != "codefly delivery key" {
		t.Fatalf("command missing: %v", err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	t.Cleanup(func() { command.SetOut(nil) })
	if err = command.Help(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"public key PEM", "host.developers", "codefly delivery key"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("help lacks %q", want)
		}
	}
}
