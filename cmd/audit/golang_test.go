package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGoCommandReturnsErrors(t *testing.T) {
	if GoCmd.RunE == nil || GoCmd.Run != nil {
		t.Fatal("audit go command must return errors through RunE")
	}
	if err := GoCmd.Args(GoCmd, []string{"extra"}); err == nil {
		t.Fatal("audit go command accepted positional arguments")
	}
}

func TestRunGoAuditUsesManagedGovulncheck(t *testing.T) {
	bin := t.TempDir()
	goPath := filepath.Join(bin, "go")
	if err := os.WriteFile(goPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	_, err := RunGoAudit(context.Background(), t.TempDir(), defaultStaleAfterDays, true)
	if err != nil {
		t.Fatalf("RunGoAudit error = %v", err)
	}
}
