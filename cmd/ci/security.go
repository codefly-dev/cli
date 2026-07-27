package ci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/gofrs/flock"
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
	response, auditErr := instance.Builder.Audit(ctx, &builderv0.AuditRequest{
		IncludeOutdated:        ciAuditIncludeOutdated,
		IncludeDevDependencies: ciAuditIncludeDev,
		FailOnVuln:             ciAuditFailOnVuln,
	})
	recordCIReportAudit(ctx, summarizeAuditResponse(response))
	if auditErr != nil {
		if purgeCorruptTrivyDB(ctx, auditErr) {
			return w.Wrapf(auditErr, "audit service: removed corrupt Trivy vulnerability DB cache, rerun the audit to re-download a clean database")
		}
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

// purgeCorruptTrivyDB removes the Trivy vulnerability database when the audit
// failed on a torn or incompletely downloaded DB, so the next run re-downloads
// a clean copy instead of mmapping the poisoned one. A truncated bbolt DB
// SIGSEGVs Trivy in FastCheck, and the fault persists across reruns until the
// file is deleted — reruns cannot otherwise self-heal. Returns true when the
// cache was purged.
func purgeCorruptTrivyDB(ctx context.Context, auditErr error) bool {
	if !isCorruptTrivyDBError(auditErr) {
		return false
	}
	cache, err := trivyCacheDir()
	if err != nil {
		return false
	}
	return removeTrivyDB(ctx, cache)
}

// removeTrivyDB deletes the vulnerability-database directories under a Trivy
// cache, leaving Trivy to re-download them on the next scan. The lock file at
// the cache root is preserved so the held lock's descriptor stays valid.
func removeTrivyDB(ctx context.Context, cache string) bool {
	// Trivy's BoltDB cache is single-writer and codefly audits services in
	// parallel; take the same cross-process lock core uses around the scan so
	// the purge cannot race a concurrent re-download.
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock := flock.New(filepath.Join(cache, ".codefly.lock"))
	locked, err := lock.TryLockContext(lockCtx, 250*time.Millisecond)
	if err != nil || !locked {
		return false
	}
	defer func() { _ = lock.Unlock() }()
	if err := os.RemoveAll(filepath.Join(cache, "db")); err != nil {
		return false
	}
	_ = os.RemoveAll(filepath.Join(cache, "java-db"))
	return true
}

// isCorruptTrivyDBError reports whether an audit failure carries the signature
// of a torn or unusable Trivy vulnerability DB — a bbolt page fault from a
// truncated mmap, or a failed/rate-limited DB download that left a partial file.
func isCorruptTrivyDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "trivy") {
		return false
	}
	for _, signature := range []string{"vulnerability db", "bbolt", "fastcheck", "sigsegv", "fatal error: fault"} {
		if strings.Contains(msg, signature) {
			return true
		}
	}
	return false
}

// trivyCacheDir mirrors the cache directory core's audit package hands to Trivy
// (os.UserCacheDir()/codefly/trivy). Core does not export it, so the CLI ci gate
// reconstructs the same path to own the cache lifecycle across runs.
func trivyCacheDir() (string, error) {
	userCache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userCache, "codefly", "trivy"), nil
}

func safeCIArtifactName(value string) string {
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, string(filepath.Separator), "-")
	value = strings.ReplaceAll(value, "..", "-")
	return value
}
