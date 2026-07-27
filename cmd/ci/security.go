package ci

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/orchestration"
	coreaudit "github.com/codefly-dev/core/agents/services/audit"
	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

var (
	ciAuditIncludeOutdated bool
	ciAuditIncludeDev      bool
	ciAuditFailOnVuln      bool
	ciSBOMIncludeDev       bool
)

func runAuditService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("ciAudit", wool.ThisField(resources.WithUnique(service)))
	instance, err := loadCIBuilder(ctx, workspace, module, service, orchestration.ValidationAudit, "dependency audit")
	if err != nil {
		return w.Wrap(err)
	}
	response, auditErr := auditWithTrivyDBRecovery(ctx, &builderv0.AuditRequest{
		IncludeOutdated:        ciAuditIncludeOutdated,
		IncludeDevDependencies: ciAuditIncludeDev,
		FailOnVuln:             ciAuditFailOnVuln,
	}, instance.Builder.Audit, coreaudit.ResetTrivyDatabases)
	recordCIReportAudit(ctx, summarizeAuditResponse(response))
	if auditErr != nil {
		return w.Wrapf(auditErr, "audit service")
	}
	if response == nil || response.GetState() == nil {
		return w.NewError("audit service: agent returned no status")
	}
	if ciAuditFailOnVuln && auditHasHighSeverity(response) {
		return w.NewError("audit found HIGH or CRITICAL vulnerabilities")
	}
	return nil
}

func runSBOMService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("ciSBOM", wool.ThisField(resources.WithUnique(service)))
	instance, err := loadCIBuilder(ctx, workspace, module, service, orchestration.ValidationSBOM, "SBOM generation")
	if err != nil {
		return w.Wrap(err)
	}
	response, err := instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{IncludeDevDependencies: ciSBOMIncludeDev})
	if err != nil {
		return w.Wrapf(err, "generate SBOM")
	}
	if response == nil || response.GetState() == nil || response.GetBom() == nil {
		return w.NewError("generate SBOM: agent returned incomplete evidence")
	}
	payload, err := coresbom.MarshalCycloneDXJSON(response.GetBom())
	if err != nil {
		return w.Wrapf(err, "encode CycloneDX")
	}
	payload = append(payload, '\n')
	identity, err := service.Identity()
	if err != nil {
		return w.Wrapf(err, "identify SBOM subject")
	}
	filename := safeCIArtifactName(identity.Module) + "--" + safeCIArtifactName(identity.Name) + ".cdx.json"
	relative, err := writeCIArtifact(workspace, filepath.Join("sbom", filename), payload)
	if err != nil {
		return w.Wrap(err)
	}
	recordCIReportArtifact(ctx, CIReportArtifact{
		Kind:      "cyclonedx-sbom",
		Path:      relative,
		MediaType: "application/vnd.cyclonedx+json",
		SHA256:    "sha256:" + resources.Hash(payload),
	})
	return nil
}

func loadCIBuilder(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, operation orchestration.ValidationOperation, label string) (*services.Instance, error) {
	instance, err := services.Load(ctx, workspace, module, service)
	if err != nil {
		return nil, fmt.Errorf("load service for %s: %w", label, err)
	}
	if advertised, supported := orchestration.ValidationOperationSupport(instance.Info, operation); advertised && !supported {
		return nil, fmt.Errorf("agent explicitly advertises %s as unsupported", label)
	}
	// Legacy agents are compatibility-probed by the typed RPC. An explicit
	// UNSUPPORTED response remains an error in the service wrapper.
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, fmt.Errorf("load builder for %s: %w", label, err)
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, fmt.Errorf("builder load for %s: %w", label, err)
	}
	return instance, nil
}

func summarizeAuditResponse(response *builderv0.AuditResponse) CIReportAudit {
	summary := CIReportAudit{}
	if response == nil {
		return summary
	}
	if response.GetState() != nil {
		summary.State = response.GetState().GetState().String()
	}
	summary.Tool = response.GetTool()
	summary.Language = response.GetLanguage()
	summary.Findings = len(response.GetFindings())
	summary.Outdated = len(response.GetOutdated())
	for _, finding := range response.GetFindings() {
		switch finding.GetSeverity() {
		case builderv0.AuditFinding_LOW:
			summary.Low++
		case builderv0.AuditFinding_MEDIUM:
			summary.Medium++
		case builderv0.AuditFinding_HIGH:
			summary.High++
		case builderv0.AuditFinding_CRITICAL:
			summary.Critical++
		}
	}
	return summary
}

func auditHasHighSeverity(response *builderv0.AuditResponse) bool {
	if response == nil {
		return false
	}
	for _, finding := range response.GetFindings() {
		if finding.GetSeverity() >= builderv0.AuditFinding_HIGH {
			return true
		}
	}
	return false
}

type auditCall func(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error)
type resetTrivyDBCall func(context.Context) error

// auditWithTrivyDBRecovery retries one audit in the same run after core resets
// a database identified by a Trivy-specific corruption signature. If the retry
// corrupts the database again, reset it once more so the poisoned cache is not
// left behind for another attempt.
func auditWithTrivyDBRecovery(ctx context.Context, request *builderv0.AuditRequest, audit auditCall, reset resetTrivyDBCall) (*builderv0.AuditResponse, error) {
	response, auditErr := audit(ctx, request)
	if auditErr == nil || !isCorruptTrivyDBError(auditErr) {
		return response, auditErr
	}
	if resetErr := reset(ctx); resetErr != nil {
		return response, fmt.Errorf("recover corrupt Trivy database: %w", errors.Join(
			auditErr,
			fmt.Errorf("reset Trivy databases: %w", resetErr),
		))
	}

	retryResponse, retryErr := audit(ctx, request)
	if retryErr == nil {
		return retryResponse, nil
	}
	retryErr = fmt.Errorf("audit retry after resetting corrupt Trivy database: %w", retryErr)
	if !isCorruptTrivyDBError(retryErr) {
		return retryResponse, retryErr
	}
	if resetErr := reset(ctx); resetErr != nil {
		return retryResponse, errors.Join(
			retryErr,
			fmt.Errorf("reset Trivy databases after failed retry: %w", resetErr),
		)
	}
	return retryResponse, fmt.Errorf("Trivy database was reset again after a corrupt audit retry: %w", retryErr)
}

// isCorruptTrivyDBError reports whether an audit failure carries the signature
// of a torn or unusable Trivy vulnerability DB — a bbolt page fault from a
// truncated mmap, or a failed/rate-limited DB download that left a partial file.
func isCorruptTrivyDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "failed to download vulnerability db") {
		return true
	}
	if !strings.Contains(msg, "bbolt") || !strings.Contains(msg, "fastcheck") {
		return false
	}
	return strings.Contains(msg, "github.com/aquasecurity/trivy-db/") ||
		strings.Contains(msg, "github.com/aquasecurity/trivy-java-db/")
}

func safeCIArtifactName(value string) string {
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, string(filepath.Separator), "-")
	value = strings.ReplaceAll(value, "..", "-")
	return value
}
