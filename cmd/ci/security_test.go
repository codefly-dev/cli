package ci

import (
	"context"
	"reflect"
	"testing"

	coreaudit "github.com/codefly-dev/core/agents/services/audit"
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

func TestCoreTrivyDatabaseRecoveryContractReturnsFinalAuditResponse(t *testing.T) {
	clean := &builderv0.AuditResponse{
		State: &builderv0.AuditStatus{State: builderv0.AuditStatus_CLEAN},
		Tool:  "trivy",
	}
	audit := func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
		return clean, nil
	}

	response, err := coreaudit.AuditWithTrivyDatabaseRecovery(context.Background(), &builderv0.AuditRequest{}, audit)
	if err != nil {
		t.Fatal(err)
	}
	if response != clean {
		t.Fatalf("response = %p, want %p", response, clean)
	}
}

func TestSafeCIArtifactNameCannotEscapeOutputDirectory(t *testing.T) {
	if got := safeCIArtifactName("../module/name"); got != "--module-name" {
		t.Fatalf("safe artifact name = %q", got)
	}
}
