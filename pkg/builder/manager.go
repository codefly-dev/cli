package builder

//
//type ActionType int
//
//const (
//	Noop ActionType = iota
//	Load
//	Init
//	Sync // Sync the service
//	Build
//	Deploy
//)
//
//// Action represents an action to be taken on a service by the runner
//type Action struct {
//	Type   ActionType
//	Unique string
//	Only   bool
//}
//
///*
//Manager is responsible for the life-cycle of a service
//- Runner is a wrapping around a service instance
//- Actions channel to affect life-cycle of the service (start, stop, restart)
//*/
//type Manager struct {
//	service  *configurations.Service
//	initOnly bool
//	instance *services.Instance
//	actions  chan Action
//
//	loaded *builderv0.LoadResponse
//	init   *builderv0.InitResponse
//	build  *builderv0.BuildResponse
//	deploy *builderv0.DeploymentResponse
//
//	dependencyEndpoints []*basev0.Endpoint
//	deployments         []*builderv0.Deployment
//	deploymentEnv       *configurations.Environment
//	providerInfos       []*basev0.ProviderInformation
//}
//
//func (manager *Manager) Unique() string {
//	return manager.service.Unique()
//}
//
//func New(ctx context.Context, service *configurations.Service) (*Manager, error) {
//	// Use buffer of size 1: more difficult but makes sure the logic is sound
//	manager := &Manager{service: service, actions: make(chan Action, 1)}
//	return manager, nil
//}
//
//func (manager *Manager) Load(ctx context.Context) error {
//	w := wool.Get(ctx).In("builder.manager::Load", wool.ThisField(manager.service))
//	instance, err := services.Load(ctx, manager.service)
//	if err != nil {
//		return w.Wrapf(err, "cannot load service instance")
//	}
//
//	err = instance.LoadBuilder(ctx)
//	if err != nil {
//		return w.Wrapf(err, "cannot load service instance")
//	}
//
//	loaded, err := instance.Builder.Load(ctx)
//	if err != nil {
//		return w.Wrapf(err, "cannot load service instance")
//	}
//	if loaded.Status != nil && loaded.Status.Status != builderv0.LoadStatus_READY {
//		return w.NewError("cannot load service instance %v", loaded.Status.Message)
//	}
//
//	Register(ctx, instance)
//
//	manager.instance = instance
//	manager.loaded = loaded
//	return nil
//}
//
//// Init the service
//func (manager *Manager) Init(ctx context.Context) error {
//	w := wool.Get(ctx).In("builder.manager::Init", wool.ThisField(manager))
//	req := &builderv0.InitRequest{DependenciesEndpoints: manager.dependencyEndpoints, ProviderInfos: manager.providerInfos}
//	init, err := manager.instance.Builder.Init(ctx, req)
//	if err != nil {
//		return w.Wrapf(err, "cannot init service instance")
//	}
//	if init.Status != nil && init.Status.Status != builderv0.InitStatus_SUCESS {
//		return w.NewError("cannot Init service instance: %v", init.Status.Message)
//	}
//	manager.init = init
//	return nil
//}
//
//func (manager *Manager) Sync(ctx context.Context) error {
//	w := wool.Get(ctx).In("service.Start", wool.ThisField(manager))
//	req := &builderv0.SyncRequest{}
//	sync, err := manager.instance.Builder.Sync(ctx, req)
//	if err != nil {
//		return w.Wrapf(err, "cannot sync service instance")
//	}
//	if sync.Status != nil && sync.Status.Status != builderv0.SyncStatus_SUCCESS {
//		return w.NewError("cannot sync service instance: %v", sync.Status.Message)
//	}
//	w.Debug("sync", wool.ResponseField(sync).Trace())
//	return nil
//}
//
//func (manager *Manager) WithEndpointDependencies(endpoints []*basev0.Endpoint) *Manager {
//	manager.dependencyEndpoints = endpoints
//	return manager
//
//}
//
//func (manager *Manager) Build(ctx context.Context) error {
//	w := wool.Get(ctx).In("service.Build", wool.ThisField(manager))
//	req := &builderv0.BuildRequest{}
//	spinner := cli.Spinner()
//	defer spinner.Stop()
//	build, err := manager.instance.Builder.Build(ctx, req)
//	if err != nil {
//		return w.Wrapf(err, "cannot build service instance")
//	}
//	if build.Status != nil && build.Status.Status != builderv0.BuildStatus_SUCCESS {
//		return w.NewError("cannot build service instance: %v", build.Status.Message)
//	}
//	manager.build = build
//	return nil
//}
//
//func (manager *Manager) Deploy(ctx context.Context) error {
//	w := wool.Get(ctx).In("service.Deploy", wool.ThisField(manager))
//	req := &builderv0.DeploymentRequest{Deployments: manager.deployments, Environment: manager.deploymentEnv.Proto()}
//	spinner := cli.Spinner()
//	defer spinner.Stop()
//	deploy, err := manager.instance.Builder.Deploy(ctx, req)
//	if err != nil {
//		return w.Wrapf(err, "cannot build service instance")
//	}
//	if deploy.Status != nil && deploy.Status.Status != builderv0.DeploymentStatus_SUCCESS {
//		return w.NewError("cannot build service instance: %v", deploy.Status.Message)
//	}
//	manager.deploy = deploy
//	return nil
//}
//
//func (manager *Manager) WithDeployments(deploys []*builderv0.Deployment) {
//	manager.deployments = deploys
//}
//
//func (manager *Manager) WithEnvironment(env *configurations.Environment) {
//	manager.deploymentEnv = env
//}
//
//func (manager *Manager) WithProviderInfos(infos []*basev0.ProviderInformation) {
//	manager.providerInfos = infos
//}
