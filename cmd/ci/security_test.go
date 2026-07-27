package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func TestSummarizeAuditResponseCountsTypedEvidence(t *testing.T) {
	response := &builderv0.AuditResponse{
		State:    &builderv0.AuditStatus{State: builderv0.AuditStatus_FINDINGS},
		Tool:     "scanner",
		Language: "typescript",
		Findings: []*builderv0.AuditFinding{
			{Severity: builderv0.AuditFinding_LOW},
			{Severity: builderv0.AuditFinding_MEDIUM},
			{Severity: builderv0.AuditFinding_HIGH},
			{Severity: builderv0.AuditFinding_CRITICAL},
		},
		Outdated: []*builderv0.OutdatedDep{{Package: "one"}, {Package: "two"}},
	}
	want := CIReportAudit{
		State: "FINDINGS", Tool: "scanner", Language: "typescript",
		Findings: 4, Low: 1, Medium: 1, High: 1, Critical: 1, Outdated: 2,
	}
	if got := summarizeAuditResponse(response); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
	if !auditHasHighSeverity(response) {
		t.Fatal("high-severity audit findings did not trip the gate")
	}
}

func TestAuditHasHighSeverityAllowsMediumAndNil(t *testing.T) {
	if auditHasHighSeverity(nil) {
		t.Fatal("nil audit response tripped the gate")
	}
	response := &builderv0.AuditResponse{Findings: []*builderv0.AuditFinding{{Severity: builderv0.AuditFinding_MEDIUM}}}
	if auditHasHighSeverity(response) {
		t.Fatal("medium finding tripped the high-severity gate")
	}
}

func TestIsCorruptTrivyDBErrorMatchesDBFaultSignatures(t *testing.T) {
	corrupt := []string{
		"builder audit failed: trivy audit failed: signal SIGSEGV: go.etcd.io/bbolt/internal/common.(*Page).FastCheck",
		"builder audit failed: trivy audit failed: exit status 2: FATAL run error: init error: DB error: failed to download vulnerability DB",
		"builder audit failed: trivy audit failed: fatal error: fault",
	}
	for _, msg := range corrupt {
		if !isCorruptTrivyDBError(errors.New(msg)) {
			t.Fatalf("did not recognize corrupt Trivy DB failure: %q", msg)
		}
	}
	healthy := []string{
		"",
		"builder audit failed: agent returned no status",
		"builder audit failed: npm audit failed: network unreachable",
		"trivy audit failed: image pull failed: not found",
	}
	for _, msg := range healthy {
		var err error
		if msg != "" {
			err = errors.New(msg)
		}
		if isCorruptTrivyDBError(err) {
			t.Fatalf("wrongly flagged as corrupt Trivy DB: %q", msg)
		}
	}
}

func TestRemoveTrivyDBClearsDatabasesAndKeepsLock(t *testing.T) {
	cache := t.TempDir()
	dbFile := filepath.Join(cache, "db", "trivy.db")
	if err := os.MkdirAll(filepath.Dir(dbFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbFile, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "java-db"), 0o700); err != nil {
		t.Fatal(err)
	}

	if !removeTrivyDB(context.Background(), cache) {
		t.Fatal("removeTrivyDB reported no purge")
	}
	if _, err := os.Stat(filepath.Join(cache, "db")); !os.IsNotExist(err) {
		t.Fatalf("db directory survived purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "java-db")); !os.IsNotExist(err) {
		t.Fatalf("java-db directory survived purge: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache root should be preserved for re-download: %v", err)
	}
}

func TestSafeCIArtifactNameCannotEscapeOutputDirectory(t *testing.T) {
	if got := safeCIArtifactName("../module/name"); got != "--module-name" {
		t.Fatalf("safe artifact name = %q", got)
	}
}
