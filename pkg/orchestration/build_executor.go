package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/wool"
)

type BuildExecutor struct {
	hub      *Hub
	builders []*BuildExecutorHelper

	repo builder.Repository
}

func (b *BuildExecutor) GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error) {
	w := wool.Get(ctx).In("GetExecutor", wool.Field("action", action))
	manager, err := b.hub.NewManager(action.Service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	helper, err := b.Helper(action)
	if err != nil {
		return nil, err
	}

	switch action.Type {
	case BuilderBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case BuilderLoad:
		return manager.BuilderDoLoad, nil
	case BuilderInit:
		return manager.BuilderDoInit, nil
	case BuilderBuild:
		return helper.Build, nil
	default:
		return nil, w.NewError("unknown action type %s for executor", action.Type)
	}
}

func (b *BuildExecutor) Helper(action Action) (*BuildExecutorHelper, error) {
	for _, bu := range b.builders {
		if bu.manager.Unique() == action.Service {
			return bu, nil
		}
	}
	return nil, fmt.Errorf("no builder found for service %s", action.Service)
}

type BuildExecutorHelper struct {
	manager IManager
}

func (h *BuildExecutorHelper) Build(ctx context.Context) (*OutputProperty, error) {
	return h.manager.BuilderDoBuild(ctx)
}

func NewBuildExecutor(ctx context.Context, hub *Hub, repo builder.Repository) (*BuildExecutor, error) {
	w := wool.Get(ctx).In("NewBuildExecutor")
	if hub == nil {
		return nil, w.NewError("hub cannot be nil")
	}
	var builders []*BuildExecutorHelper
	for _, manager := range hub.managers {
		builders = append(builders, &BuildExecutorHelper{manager: manager})
	}
	return &BuildExecutor{hub: hub, builders: builders, repo: repo}, nil
}
