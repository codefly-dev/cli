package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/services"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

type Mode string

const (
	RunMode      Mode = "run"
	TestMode     Mode = "test"
	LintMode     Mode = "lint"
	CompileMode  Mode = "compile"
	SyncMode     Mode = "sync"
	BuildMode    Mode = "build"
	DeployMode   Mode = "deploy"
	SnapshotMode Mode = "snapshot"
)

type IManager interface {
	Unique() string

	// Builder interface

	BuilderDoInit(ctx context.Context) (*OutputProperty, error)
	BuilderDoLoad(ctx context.Context) (*OutputProperty, error)
	BuilderDoBuild(ctx context.Context) (*OutputProperty, error)
	BuilderDoSync(ctx context.Context) (*OutputProperty, error)
	BuilderDoDeploy(ctx context.Context) (*OutputProperty, error)

	// Runner interface

	RunnerDoLoad(ctx context.Context) (*OutputProperty, error)
	RunnerDoInit(ctx context.Context) (*OutputProperty, error)
	RunnerDoStart(ctx context.Context) (*OutputProperty, error)
	RunnerDoBuild(ctx context.Context) (*OutputProperty, error)
	RunnerDoLint(ctx context.Context) (*OutputProperty, error)
	RunnerDoTest(ctx context.Context) (*OutputProperty, error)
	RunnerDoStop(ctx context.Context) (*OutputProperty, error)
	RunnerDoDestroy(ctx context.Context) (*OutputProperty, error)

	// RunnerTestResponse returns the structured response from the last Test
	// RPC dispatched by this manager's runner, or nil if untested.
	RunnerTestResponse() *runtimev0.TestResponse

	DoSetCallback(seed func(ctx context.Context, action Action) error)
	// DoSetFailureSink registers a callback that the Runner.Follow loop
	// invokes when a started service is observed dead. Used by Flow to
	// fan failures into its top-level shutdown channel.
	DoSetFailureSink(sink func(unique, msg string))
}

/*
Manager is responsible is a wrapper around a service instance:
- keeps around request Dependencies
- keeps around ConfigurationManager information
*/
type Manager struct {
	service *resources.Service
	module  *resources.Module

	world *World

	Runner *Runner

	Builder *Builder

	initOnly bool

	load *runtimev0.LoadResponse
	init *runtimev0.InitResponse

	dependencyEndpoints []*basev0.Endpoint
	networkMappings     []*basev0.NetworkMapping
}

func (manager *Manager) BuilderDoInit(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Init(ctx)
}

func (manager *Manager) BuilderDoLoad(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Load(ctx)
}

func (manager *Manager) BuilderDoBuild(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Build(ctx)
}

func (manager *Manager) BuilderDoSync(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Sync(ctx)
}

func (manager *Manager) BuilderDoDeploy(ctx context.Context) (*OutputProperty, error) {
	return manager.Builder.Deploy(ctx)
}

func (manager *Manager) BuilderSyncResponse() *builderv0.SyncResponse {
	if manager.Builder == nil {
		return nil
	}
	return manager.Builder.SyncResponse()
}

func (manager *Manager) BuilderSyncSkipped() bool {
	if manager.Builder == nil {
		return false
	}
	return manager.Builder.SyncSkipped()
}

func (manager *Manager) BuilderDeploymentOutput() *builderv0.DeploymentOutput {
	if manager.Builder == nil {
		return nil
	}
	return manager.Builder.DeploymentOutput()
}

func (manager *Manager) BuilderImageDigest() string {
	if manager.Builder == nil {
		return ""
	}
	return manager.Builder.ImageDigest()
}

func (manager *Manager) RunnerDoLoad(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Load(ctx)
}

func (manager *Manager) RunnerDoInit(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Init(ctx)
}

func (manager *Manager) RunnerDoStart(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Start(ctx)
}

func (manager *Manager) RunnerDoBuild(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Build(ctx)
}

func (manager *Manager) RunnerDoLint(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Lint(ctx)
}

func (manager *Manager) RunnerDoTest(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Test(ctx)
}

func (manager *Manager) RunnerDoStop(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Stop(ctx)
}

func (manager *Manager) RunnerDoDestroy(ctx context.Context) (*OutputProperty, error) {
	return manager.Runner.Destroy(ctx)
}

func (manager *Manager) RunnerTestResponse() *runtimev0.TestResponse {
	if manager.Runner == nil {
		return nil
	}
	return manager.Runner.TestResponse()
}

func (manager *Manager) DoSetCallback(callback func(ctx context.Context, action Action) error) {
	manager.SetCallback(callback)
}

func (manager *Manager) Unique() string {
	return resources.WithUnique(manager.service).Unique()
}

func New(ctx context.Context, module *resources.Module, service *resources.Service, world *World) (*Manager, error) {
	w := wool.Get(ctx).In("hub.New", wool.ThisField(resources.WithUnique(service)))

	manager := &Manager{service: service, module: module, world: world}
	err := manager.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	return manager, nil
}

func (manager *Manager) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("hub.New", wool.ThisField(manager))

	instance, err := services.Load(ctx, manager.world.Workspace, manager.module, manager.service)
	if err != nil {
		return w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("load agent", wool.Field("agent-pid", instance.ProcessInfo.AgentPID))

	switch manager.world.Mode {
	case RunMode, TestMode, LintMode, CompileMode:
		w.Debug("load runtime")
		err = instance.LoadRuntime(ctx, true)
		if err != nil {
			return w.Wrapf(err, "cannot load service instance")
		}
		manager.Runner, err = NewRunner(ctx, instance, manager.world)
		if err != nil {
			return w.Wrapf(err, "cannot create runner")
		}
		err = manager.Runner.Follow(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot follow service instance")
		}
		return nil
	case BuildMode, SyncMode, DeployMode, SnapshotMode:
		err = instance.LoadBuilder(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load service builder instance")
		}
		manager.Builder, err = NewBuilder(ctx, instance, manager.world)
		if err != nil {
			return w.Wrapf(err, "cannot create builder")
		}
		return nil
	}
	return w.NewError("unknown mode %s", manager.world.Mode)
}

func (manager *Manager) SetCallback(f Callback) {
	if manager.Runner == nil {
		return
	}
	manager.Runner.callback = f
}

func (manager *Manager) DoSetFailureSink(sink func(unique, msg string)) {
	if manager.Runner == nil {
		return
	}
	manager.Runner.failureSink = sink
}

type Hub struct {
	managers []IManager
}

func (hub *Hub) NewManager(unique string) (IManager, error) {
	for _, manager := range hub.managers {
		if manager.Unique() == unique {
			return manager, nil
		}
	}
	return nil, fmt.Errorf("no manager found for %s", unique)
}

type NoOpManager struct {
	service *resources.Service
}

// environmentOnlyManager represents an excluded root whose process must not
// start, while still materializing the SDK environment that process would
// receive. Dependencies run through their real managers; the export callback
// executes at the root's RuntimeStart barrier, after those dependencies have
// published their final endpoints and runtime configurations.
//
// ARCHITECTURE: This is intentionally not a Runner. Creating a Runner loads the
// root agent and eventually starts its user binary, defeating --exclude-root.
// Embedding NoOpManager preserves the dependency playbook shape without
// inventing an agent lifecycle for a process that is not allowed to exist.
type environmentOnlyManager struct {
	NoOpManager
	export func(context.Context) error
}

// RunnerDoStart publishes the excluded root's environment at the same
// dependency barrier where a real Runner would append final dependency state.
func (manager environmentOnlyManager) RunnerDoStart(ctx context.Context) (*OutputProperty, error) {
	if manager.export != nil {
		if err := manager.export(ctx); err != nil {
			return nil, fmt.Errorf("export excluded root environment: %w", err)
		}
	}
	return OnInit(), nil
}

func (n NoOpManager) RunnerDoTest(ctx context.Context) (*OutputProperty, error) {
	return nil, nil
}

func (n NoOpManager) RunnerDoBuild(ctx context.Context) (*OutputProperty, error) {
	return OnInit(), nil
}

func (n NoOpManager) RunnerDoLint(ctx context.Context) (*OutputProperty, error) {
	return OnInit(), nil
}

func (n NoOpManager) RunnerDoStop(ctx context.Context) (*OutputProperty, error) {
	return nil, nil
}

func (n NoOpManager) RunnerDoDestroy(ctx context.Context) (*OutputProperty, error) {
	return nil, nil
}

func (n NoOpManager) RunnerTestResponse() *runtimev0.TestResponse {
	return nil
}

func (n NoOpManager) Unique() string {
	return resources.WithUnique(n.service).Unique()
}

func (n NoOpManager) BuilderDoInit(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoLoad(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoBuild(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoSync(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) BuilderDoDeploy(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) RunnerDoLoad(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) RunnerDoInit(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) RunnerDoStart(ctx context.Context) (*OutputProperty, error) {
	return &OutputProperty{OnInit: true}, nil
}

func (n NoOpManager) DoSetCallback(seed func(ctx context.Context, action Action) error) {
}

func (n NoOpManager) DoSetFailureSink(sink func(unique, msg string)) {
}

func (n NoOpManager) Stop(ctx context.Context) error {
	return nil
}
