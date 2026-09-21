package gitops

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/core/resources"
)

func rejectUnboundPublication(ctx context.Context, workspace *resources.Workspace, module string) error {
	if err := selectionguard.RejectUnboundExecution(workspace.Dir()); err != nil {
		return err
	}
	for _, reference := range workspace.Modules {
		if reference.Name != module {
			continue
		}
		resolved, err := workspace.ResolveModule(ctx, reference)
		if err != nil {
			return err
		}
		if resolved.Dir == "" {
			return fmt.Errorf("%s: publication cannot establish released-module execution bindings without local metadata: %w", module, selectionguard.ErrUnboundExecution)
		}
		return selectionguard.RejectUnboundExecution(resolved.Dir)
	}
	return nil
}
