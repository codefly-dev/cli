package ci

import (
	"context"
	"errors"
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
		"builder audit failed\nfatal error: fault\n[signal SIGSEGV]\ngo.etcd.io/bbolt/internal/common.(*Page).FastCheck\ngithub.com/aquasecurity/trivy-db/pkg/db.Config.forEach",
		"builder audit failed: exit status 2: FATAL run error: init error: DB error: failed to download vulnerability DB",
		"builder audit failed\nfatal error: fault\ngo.etcd.io/bbolt/internal/common.(*Page).FastCheck\ngithub.com/aquasecurity/trivy-java-db/pkg/db.Config.Get",
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
		"builder audit failed: npm audit failed: signal SIGSEGV",
		"builder audit failed: go.etcd.io/bbolt/internal/common.(*Page).FastCheck",
		"builder audit failed: trivy audit failed: fatal error: fault",
		"builder audit failed: trivy audit failed: signal SIGSEGV",
		"builder audit failed: trivy audit failed: bbolt FastCheck: github.com/aquasecurity/trivy/pkg/cache",
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

func TestAuditWithTrivyDBRecoveryRetriesAfterReset(t *testing.T) {
	calls := 0
	clean := &builderv0.AuditResponse{
		State: &builderv0.AuditStatus{State: builderv0.AuditStatus_CLEAN},
		Tool:  "trivy",
	}
	audit := func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
		calls++
		if calls == 1 {
			return nil, corruptTrivyDBTestError()
		}
		return clean, nil
	}
	resets := 0
	reset := func(context.Context) error {
		resets++
		return nil
	}

	response, err := auditWithTrivyDBRecovery(context.Background(), &builderv0.AuditRequest{}, audit, reset)
	if err != nil {
		t.Fatal(err)
	}
	if response != clean || calls != 2 || resets != 1 {
		t.Fatalf("response = %p, calls = %d, resets = %d", response, calls, resets)
	}
}

func TestAuditWithTrivyDBRecoverySurfacesResetFailureWithoutRetry(t *testing.T) {
	calls := 0
	auditErr := corruptTrivyDBTestError()
	audit := func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
		calls++
		return nil, auditErr
	}
	resetErr := errors.New("permission denied removing root-owned database")
	resets := 0
	reset := func(context.Context) error {
		resets++
		return resetErr
	}

	_, err := auditWithTrivyDBRecovery(context.Background(), &builderv0.AuditRequest{}, audit, reset)
	if !errors.Is(err, auditErr) || !errors.Is(err, resetErr) {
		t.Fatalf("recovery error did not preserve audit and reset failures: %v", err)
	}
	if calls != 1 || resets != 1 {
		t.Fatalf("calls = %d, resets = %d; reset failure must stop retry", calls, resets)
	}
}

func TestAuditWithTrivyDBRecoveryResetsAgainAfterCorruptRetry(t *testing.T) {
	calls := 0
	audit := func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
		calls++
		return nil, corruptTrivyDBTestError()
	}
	resets := 0
	reset := func(context.Context) error {
		resets++
		return nil
	}

	_, err := auditWithTrivyDBRecovery(context.Background(), &builderv0.AuditRequest{}, audit, reset)
	if err == nil || calls != 2 || resets != 2 {
		t.Fatalf("error = %v, calls = %d, resets = %d", err, calls, resets)
	}
}

func TestAuditWithTrivyDBRecoverySurfacesFinalResetFailure(t *testing.T) {
	calls := 0
	auditErr := corruptTrivyDBTestError()
	audit := func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
		calls++
		return nil, auditErr
	}
	finalResetErr := errors.New("final reset failed")
	resets := 0
	reset := func(context.Context) error {
		resets++
		if resets == 2 {
			return finalResetErr
		}
		return nil
	}

	_, err := auditWithTrivyDBRecovery(context.Background(), &builderv0.AuditRequest{}, audit, reset)
	if !errors.Is(err, auditErr) || !errors.Is(err, finalResetErr) {
		t.Fatalf("final recovery error did not preserve retry and reset failures: %v", err)
	}
	if calls != 2 || resets != 2 {
		t.Fatalf("calls = %d, resets = %d", calls, resets)
	}
}

func TestAuditWithTrivyDBRecoveryLeavesUnrelatedFailuresUntouched(t *testing.T) {
	auditErr := errors.New("builder audit failed: trivy audit failed: signal SIGSEGV")
	calls := 0
	audit := func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
		calls++
		return nil, auditErr
	}
	resets := 0
	reset := func(context.Context) error {
		resets++
		return nil
	}

	_, err := auditWithTrivyDBRecovery(context.Background(), &builderv0.AuditRequest{}, audit, reset)
	if !errors.Is(err, auditErr) || calls != 1 || resets != 0 {
		t.Fatalf("error = %v, calls = %d, resets = %d", err, calls, resets)
	}
}

func corruptTrivyDBTestError() error {
	return errors.New("builder audit failed: failed to download vulnerability DB")
}

func TestSafeCIArtifactNameCannotEscapeOutputDirectory(t *testing.T) {
	if got := safeCIArtifactName("../module/name"); got != "--module-name" {
		t.Fatalf("safe artifact name = %q", got)
	}
}
