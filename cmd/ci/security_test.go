package ci

import (
	"context"
	"reflect"
	"strings"
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

func TestImageEvidenceFilenameKeysOnTheScanIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	amd64 := &builderv0.ImageSBOM{Digest: digest, Platform: "linux/amd64"}
	arm64 := &builderv0.ImageSBOM{Digest: digest, Platform: "linux/arm64"}
	second := &builderv0.ImageSBOM{Digest: other, Platform: "linux/amd64"}

	if imageEvidenceFilename(amd64) == imageEvidenceFilename(arm64) {
		t.Fatal("two platforms of one multi-architecture image collided on one filename")
	}
	if imageEvidenceFilename(amd64) == imageEvidenceFilename(second) {
		t.Fatal("two service-owned images collided on one filename")
	}
	if got, want := imageEvidenceFilename(&builderv0.ImageSBOM{Digest: digest, Platform: "linux/amd64"}), imageEvidenceFilename(amd64); got != want {
		t.Fatalf("one scanned image did not deduplicate: %q vs %q", got, want)
	}
	if name := imageEvidenceFilename(amd64); strings.Contains(name, "/") {
		t.Fatalf("platform separator escaped into a path: %q", name)
	}
}

func TestImageEvidenceSubjectsKeepEveryServiceAssociation(t *testing.T) {
	image := &builderv0.ImageSBOM{Subjects: []*builderv0.ImageSubject{
		{Service: "management/worker", Role: "runtime", Reference: "repo/worker:v1"},
		{Service: "billing/accounts", Role: "migration"},
	}}
	want := []CIReportImageSubject{
		{Service: "management/worker", Role: "runtime", Reference: "repo/worker:v1"},
		{Service: "billing/accounts", Role: "migration"},
	}
	if got := imageEvidenceSubjects(image); !reflect.DeepEqual(got, want) {
		t.Fatalf("subjects = %#v, want %#v", got, want)
	}
}
