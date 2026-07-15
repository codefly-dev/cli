package ci

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

// Silent services in the CLI
var silent []string

// Scope for the runtime: affect ports to avoid conflict with run
// Useful for testing/CI
var scope string

// Runtime context
var runtimeContext string

// load only mode
var loadOnly bool

// init only mode
var initOnly bool

type Action func(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error

type flowStopper interface {
	Stop() error
}

// runAndStopFlow guarantees that every CI flow is stopped exactly once. Both
// the operation and teardown errors are retained so a cleanup failure cannot
// hide the original build/test/deploy failure (or vice versa).
func runAndStopFlow(flow flowStopper, action func() error) (result error) {
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		defer services.ClearAgents()
		if flow == nil {
			return nil
		}
		return flow.Stop()
	}
	defer func() {
		if err := stop(); err != nil {
			result = errors.Join(result, fmt.Errorf("cannot stop flow: %w", err))
		}
	}()

	if action == nil {
		return nil
	}
	return action()
}

func stopFlowAfterError(flow *orchestration.Flow, err error) error {
	return runAndStopFlow(flow, func() error { return err })
}

func CI(ctx context.Context, workspace *resources.Workspace, action Action) error {
	w := wool.Get(ctx).In("deployCI")
	// Load dependencies
	deps, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "Cannot load dependencies")
	}
	entryPoints, err := deps.EntryPoints(ctx)
	if err != nil {
		return w.Wrapf(err, "Cannot get entrypoints")
	}
	alreadyRan := make(map[string]bool)
	for _, entry := range entryPoints {
		// Get a topological order of the services up to entryPoint
		order, err := deps.OrderTo(ctx, entry.Unique)
		if err != nil {
			return w.Wrapf(err, "Cannot get order to <%s>", entry.Unique)
		}
		w.Debug("Order", wool.Field("to", entry.Unique), wool.Field("order", order))
		order = append(order, entry)
		for _, svc := range order {
			if alreadyRan[svc.Unique] {
				continue
			}
			w.Info("Handling services", wool.Field("service", svc.Unique))

			ref, err := resources.ParseServiceWithOptionalModule(svc.Unique)
			if err != nil {
				return w.Wrapf(err, "Cannot parse service <%s>", svc.Unique)
			}

			module, err := workspace.LoadModuleFromName(ctx, ref.Module)
			if err != nil {
				return w.Wrapf(err, "Cannot load module <%s>", ref.Module)
			}
			service, err := module.LoadServiceFromName(ctx, ref.Name)
			if err != nil {
				return w.Wrapf(err, "Cannot load service <%s>", svc.Unique)
			}

			service.WithModule(module.Name)

			err = action(ctx, workspace, module, service)
			if err != nil {
				return w.Wrapf(err, "Cannot run CI %T for service <%s>", action, svc.Unique)
			}
			alreadyRan[svc.Unique] = true
		}
	}
	return nil
}
