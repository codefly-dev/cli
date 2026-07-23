package ci

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/orchestration"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func runSyncDriftService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("ciSyncDrift", wool.ThisField(resources.WithUnique(service)))
	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return w.Wrapf(err, "select sync drift environment")
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.SyncMode)
	if err != nil {
		return w.Wrapf(err, "create sync drift flow")
	}
	flow.WithSyncRequest(&builderv0.SyncRequest{DryRun: true})
	if err := flow.InitManagers(ctx); err != nil {
		return stopFlowAfterError(flow, w.Wrapf(err, "initialize sync drift managers"))
	}
	if err := flow.Load(ctx); err != nil {
		return stopFlowAfterError(flow, w.Wrapf(err, "load sync drift flow"))
	}
	return runAndStopFlow(flow, func() error {
		if err := flow.Sync(ctx); err != nil {
			return w.Wrapf(err, "run non-mutating sync")
		}
		if flow.OriginSyncSkipped() {
			recordCIReportSkip(ctx, reportReasonAgentNoSyncCapability)
			return nil
		}
		response := flow.OriginSyncResponse()
		if response == nil || response.GetState() == nil {
			return w.NewError("sync drift agent returned no status")
		}
		changed := sortedUnique(response.GetChangedFiles())
		recordCIReportDrift(ctx, changed)
		if len(changed) == 0 {
			return nil
		}
		shown := changed
		if len(shown) > 10 {
			shown = shown[:10]
		}
		message := strings.Join(shown, ", ")
		if len(changed) > len(shown) {
			message += fmt.Sprintf(" (and %d more)", len(changed)-len(shown))
		}
		return w.NewError("generated source drift detected in %d file(s): %s", len(changed), message)
	})
}
