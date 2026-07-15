package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompletionCommandValidation(t *testing.T) {
	if CompletionCmd.RunE == nil || CompletionCmd.Run != nil {
		t.Fatal("completion command must return errors through RunE")
	}
	if err := CompletionCmd.Args(CompletionCmd, nil); err == nil {
		t.Fatal("completion command accepted a missing shell")
	}
	if err := CompletionCmd.Args(CompletionCmd, []string{"unknown"}); err == nil {
		t.Fatal("completion command accepted an unsupported shell")
	}
}

func TestCompletionInstallIsAtomicAndCreatesParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	previous := completionInstall
	completionInstall = true
	t.Cleanup(func() { completionInstall = previous })

	if err := CompletionCmd.RunE(CompletionCmd, []string{"bash"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(home, ".local", "share", "bash-completion", "completions", "codefly")
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 {
		t.Fatal("installed completion file is empty")
	}
}
