package manager

import (
	"context"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/wool"
)

func BasicNext(ctx context.Context, dependencies *architecture.ServiceDependencies, change *OutputProperty, action Action, nextType ActionType) ([]Action, error) {
	w := wool.Get(ctx).In("SyncPolicy.BasicNext")
	w.Debug("computing next action with output", wool.Field("property", change))
	if change.OnInit {
		required, err := dependencies.OrderTo(ctx, action.Service)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get required")
		}
		next := action.NextFor(nextType, required...)
		next = append(next, action.Next(nextType))
		return next, nil
	}
	if change.IndependentUpdate {
		next := []Action{action.Next(nextType)}
		return next, nil
	}
	if change.UpdateWithRequiredPropagation {
		deps, err := dependencies.DirectDependents(ctx, action.Service)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get required")
		}
		// We execute the same action on all dependents
		w.Debug("propagating", wool.Field("service", action.Service), wool.Field("direct dependents", deps))
		next := action.NextFor(action.Type, deps...)
		next = append(next, action.Next(nextType))
		next = append(next, action.NextFor(nextType, deps...)...)
		w.Debug("propagating", wool.Field("service", action.Service), wool.Field("next", next))
		return next, nil
	}
	return nil, nil
}
