package manager

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/core/wool"
)

type BuildExecutor struct {
	hub      *Hub
	builders []*BuildExecutorHelper

	repo builder.Repository
	ci   bool
}

func (b *BuildExecutor) GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error) {
	w := wool.Get(ctx).In("GetExecutor", wool.Field("action", action))
	manager, err := b.hub.Manager(action.Service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	helper, err := b.Helper(action)

	switch action.Type {
	case BuilderBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case BuilderLoad:
		return manager.Builder.Load, nil
	case BuilderInit:
		return manager.Builder.Init, nil
	case BuilderBuild:
		return helper.Build, nil
	default:
		return nil, w.NewError("unknown action type %s for executor", action.Type)
	}
}

func (b *BuildExecutor) Helper(action Action) (*BuildExecutorHelper, error) {
	for _, bu := range b.builders {
		if bu.manager.service.Unique() == action.Service {
			return bu, nil
		}
	}
	return nil, fmt.Errorf("no builder found for service %s", action.Service)
}

type BuildExecutorHelper struct {
	manager *Manager
}

func (h *BuildExecutorHelper) Build(ctx context.Context) (*OutputProperty, error) {
	return h.manager.Builder.Build(ctx)
}

func NewBuildExecutor(ctx context.Context, hub *Hub, repo builder.Repository, ci bool) (*BuildExecutor, error) {
	var builders []*BuildExecutorHelper
	for _, manager := range hub.managers {
		builders = append(builders, &BuildExecutorHelper{manager: manager})
	}
	return &BuildExecutor{hub: hub, builders: builders, repo: repo, ci: ci}, nil
}
