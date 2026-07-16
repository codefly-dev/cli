package ci

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/orchestration"
	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

var (
	ciAuditIncludeOutdated bool
	ciAuditFailOnVuln      bool
	ciSBOMIncludeDev       bool
)

func runAuditService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("ciAudit", wool.ThisField(resources.WithUnique(service)))
	instance, err := loadCIBuilder(ctx, workspace, module, service, orchestration.ValidationAudit, "dependency audit")
	if err != nil {
		return w.Wrap(err)
	}
	response, auditErr := instance.Builder.Audit(ctx, &builderv0.AuditRequest{
		IncludeOutdated: ciAuditIncludeOutdated,
		FailOnVuln:      ciAuditFailOnVuln,
	})
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

func safeCIArtifactName(value string) string {
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, string(filepath.Separator), "-")
	value = strings.ReplaceAll(value, "..", "-")
	return value
}
