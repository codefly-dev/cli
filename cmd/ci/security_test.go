package ci

import (
	"context"
	"reflect"
	"strings"
	"testing"

	coreaudit "github.com/codefly-dev/core/agents/services/audit"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
)

func TestSummarizeAuditResponseCountsTypedEvidence(t *testing.T) {
	response := &builderv1.AuditResponse{
		State:    &builderv1.AuditStatus{State: builderv1.AuditStatus_FINDINGS},
		Tool:     "scanner",
		Language: "typescript",
		Findings: []*builderv1.AuditFinding{
			{Severity: builderv1.AuditFinding_LOW},
			{Severity: builderv1.AuditFinding_MEDIUM},
			{Severity: builderv1.AuditFinding_HIGH},
			{Severity: builderv1.AuditFinding_CRITICAL},
		},
		Outdated: []*builderv1.OutdatedDep{{Package: "one"}, {Package: "two"}},
	}
	want := ReportAudit{
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
	response := &builderv1.AuditResponse{Findings: []*builderv1.AuditFinding{{Severity: builderv1.AuditFinding_MEDIUM}}}
	if auditHasHighSeverity(response) {
		t.Fatal("medium finding tripped the high-severity gate")
	}
}

func TestCoreTrivyDatabaseRecoveryContractReturnsFinalAuditResponse(t *testing.T) {
	clean := &builderv1.AuditResponse{
		State: &builderv1.AuditStatus{State: builderv1.AuditStatus_CLEAN},
		Tool:  "trivy",
	}
	audit := func(context.Context, *builderv1.AuditRequest) (*builderv1.AuditResponse, error) {
		return clean, nil
	}

	response, err := coreaudit.AuditWithTrivyDatabaseRecovery(context.Background(), &builderv1.AuditRequest{}, audit)
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
	amd64 := &builderv1.ImageSBOM{Digest: digest, Platform: "linux/amd64"}
	arm64 := &builderv1.ImageSBOM{Digest: digest, Platform: "linux/arm64"}
	second := &builderv1.ImageSBOM{Digest: other, Platform: "linux/amd64"}

	if imageEvidenceFilename(amd64) == imageEvidenceFilename(arm64) {
		t.Fatal("two platforms of one multi-architecture image collided on one filename")
	}
	if imageEvidenceFilename(amd64) == imageEvidenceFilename(second) {
		t.Fatal("two service-owned images collided on one filename")
	}
	if got, want := imageEvidenceFilename(&builderv1.ImageSBOM{Digest: digest, Platform: "linux/amd64"}), imageEvidenceFilename(amd64); got != want {
		t.Fatalf("one scanned image did not deduplicate: %q vs %q", got, want)
	}
	if name := imageEvidenceFilename(amd64); strings.Contains(name, "/") {
		t.Fatalf("platform separator escaped into a path: %q", name)
	}
}

func TestImageEvidenceAssociationsKeepEveryServiceClaim(t *testing.T) {
	image := &builderv1.ImageSBOM{Subjects: []*builderv1.ImageSubject{
		{Service: "management/worker", Role: "runtime", Reference: "repo/worker:v1"},
		{Service: "billing/accounts", Role: "migration"},
	}}
	want := []ImageAssociation{
		{Service: "management/worker", Role: "runtime", Reference: "repo/worker:v1"},
		{Service: "billing/accounts", Role: "migration"},
	}
	if got := imageEvidenceAssociations(image); !reflect.DeepEqual(got, want) {
		t.Fatalf("associations = %#v, want %#v", got, want)
	}
}
