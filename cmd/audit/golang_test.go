package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// staleSuppressions only warns on a reviewed date it cannot parse, so a typo
// introduced while bumping one silently exempts that entry from the review
// clock instead of failing the gate.
func TestCommittedSuppressionsAreWellFormed(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	supps, path, err := LoadSuppressions(wd)
	if err != nil {
		t.Fatalf("LoadSuppressions error = %v", err)
	}
	if path == "" {
		t.Fatal("no .govulncheck.yaml found walking up from cmd/audit")
	}
	for _, s := range supps {
		if s.ID == "" || s.Module == "" || s.Reason == "" {
			t.Errorf("%s: entry %+v is missing id, module or reason", path, s)
			continue
		}
		if _, err := time.Parse("2006-01-02", s.Reviewed); err != nil {
			t.Errorf("%s: %s has an unparseable reviewed date %q", path, s.ID, s.Reviewed)
		}
	}
}
