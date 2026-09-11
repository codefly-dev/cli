package ci

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/integrity"
	"github.com/codefly-dev/core/resources"
)

// runVerifyWorkspace verifies both the current hashes and ownership removed since
// the baseline. Service selection must not decide whether this evidence exists.
func runVerifyWorkspace(ctx context.Context, workspace *resources.Workspace, plan *Plan) error {
	report, verifyErr := integrity.VerifyBase(ctx, workspace, plan.integrityModules()...)
	if len(plan.IntegrityInputs) > 0 {
		transitionErr := verifyPlanOwnership(ctx, workspace, plan, &report)
		verifyErr = errors.Join(verifyErr, transitionErr)
	}
	recordCIReportIntegrity(ctx, summarizeIntegrityReport(report))
	return verifyErr
}

func verifyPlanOwnership(ctx context.Context, workspace *resources.Workspace, plan *Plan, report *integrity.BaseReport) error {
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return err
	}
	var errs error
	for _, name := range plan.integrityModules() {
		for _, module := range modules {
			if module.Name != name {
				continue
			}
			err := verifyModuleOwnership(ctx, workspace, module, plan.Base)
			if err == nil {
				continue
			}
			errs = errors.Join(errs, fmt.Errorf("module %s integrity ownership: %w", name, err))
			for i := range report.Modules {
				if report.Modules[i].Module == name {
					report.Modules[i].Error = strings.TrimSpace(report.Modules[i].Error + "\n" + err.Error())
				}
			}
		}
	}
	return errs
}

func verifyModuleOwnership(ctx context.Context, workspace *resources.Workspace, module *resources.Module, base string) error {
	if base == "" {
		if isCIEnvironment() {
			return fmt.Errorf("--base is required to validate manifest ownership in CI")
		}
		base = gitHeadRevision
	}
	root, err := gitRoot(ctx, workspace.Dir())
	if err != nil {
		return fmt.Errorf("resolve manifest ownership baseline: %w", err)
	}
	// Resolve the parent, not the file, so deletion or replacement of the
	// manifest cannot change which historical file supplies its ownership.
	path := filepath.Join(cleanAbs(filepath.Join(module.Dir(), "tools")), "base-manifest.json")
	if !pathWithin(path, cleanAbs(root)) {
		return fmt.Errorf("manifest is outside the repository: %s", path)
	}
	relative, err := filepath.Rel(cleanAbs(root), path)
	if err != nil {
		return err
	}
	revision, err := gitOutput(ctx, root, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve manifest ownership baseline: %w", err)
	}
	entries, err := gitOutput(ctx, root, "ls-tree", "--format=%(objectmode) %(objectname)", strings.TrimSpace(string(revision)), "--", ":(literal)"+filepath.ToSlash(relative))
	if err != nil {
		return fmt.Errorf("read manifest ownership baseline: %w", err)
	}
	if len(entries) == 0 {
		// Git proved this is a newly introduced manifest.
		return nil
	}
	fields := strings.Fields(string(entries))
	if len(fields) != 2 || (fields[0] != "100644" && fields[0] != "100755") {
		return fmt.Errorf("baseline manifest must be a regular file: %s", relative)
	}
	previous, err := gitOutput(ctx, root, "cat-file", "blob", fields[1])
	if err != nil {
		return fmt.Errorf("read manifest ownership baseline: %w", err)
	}
	return integrity.VerifyBaseManifestTransition(module.Dir(), previous)
}
