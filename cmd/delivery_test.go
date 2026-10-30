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

func TestDeliveryApplyCommandIsRegisteredAndExplained(t *testing.T) {
	command, args, err := RootCmd.Find([]string{"delivery", "apply"})
	if err != nil || len(args) != 0 || command == nil || command.CommandPath() != "codefly delivery apply" {
		t.Fatalf("command missing: %v", err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	t.Cleanup(func() { command.SetOut(nil) })
	if err = command.Help(); err != nil {
		t.Fatal(err)
	}
	// Help must stand alone: what the verb sends, why the credential is not in
	// the document, and that only this interface takes the pair.
	for _, want := range []string{
		"ApplyManifest", "both signed halves", "NO credential", "read from --identity-file at execution",
		"--documents", "--address", "--identity-file", "--budget",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("help lacks %q", want)
		}
	}
}

// Every flag the Job depends on is required, and the refusal says what it is
// for: a Job that ran with one missing would otherwise fail inside the dial or
// read an empty directory.
func TestDeliveryApplyRefusesEachMissingFlagByName(t *testing.T) {
	command, _, err := RootCmd.Find([]string{"delivery", "apply"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--documents", "--address", "--identity-file"} {
		t.Run(want, func(t *testing.T) {
			for _, flag := range []string{"documents", "address", "identity-file"} {
				value := "set"
				if "--"+flag == want {
					value = ""
				}
				if err := command.Flags().Set(flag, value); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			t.Cleanup(func() { command.SetOut(nil); command.SetErr(nil) })
			err := command.RunE(command, nil)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want a refusal naming %s, got %v", want, err)
			}
		})
	}
}

func TestDeliveryPrepareCommandIsRegisteredAndExplained(t *testing.T) {
	command, args, err := RootCmd.Find([]string{"delivery", "prepare"})
	if err != nil || len(args) != 0 || command == nil || command.CommandPath() != "codefly delivery prepare" {
		t.Fatalf("command missing: %v", err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	t.Cleanup(func() { command.SetOut(nil) })
	if err = command.Help(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"BOTH signed halves", "no roles field", "refused by name", "carry no credential",
		"--overlay", "--out", "--env", "--developer",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("help lacks %q", want)
		}
	}
}
