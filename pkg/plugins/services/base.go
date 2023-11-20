package services

import (
	"context"
	"embed"
	"fmt"
	"path"

	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/cli/pkg/plugins/communicate"
	"github.com/codefly-dev/cli/pkg/plugins/endpoints"
	"github.com/codefly-dev/cli/pkg/plugins/helpers/code"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	runtimev1 "github.com/codefly-dev/cli/proto/v1/services/runtime"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

func PluginLogger(ctx context.Context) *plugins.PluginLogger {
	return ctx.Value(shared.Plugin).(*plugins.PluginLogger)
}

func ServiceLogger(ctx context.Context) *plugins.ServiceLogger {
	return ctx.Value(shared.Service).(*plugins.ServiceLogger)
}

type Base struct {
	// Plugin
	Plugin *configurations.Plugin

	// State
	Identity              *v1.ServiceIdentity
	Location              string
	ConfigurationLocation string
	Configuration         *configurations.Service

	// Endpoints
	Endpoints []*corev1.Endpoint

	// Runtime
	Status InformationStatus

	// Loggers
	ServiceLogger *plugins.ServiceLogger
	PluginLogger  *plugins.PluginLogger

	// Communication Manager
	CommunicationClientManager *communicate.ClientManager

	// Code Watcher
	Watcher *code.Watcher
	Events  chan code.Change

	// Internal
	ctx context.Context
}

func NewServiceBase(plugin *configurations.Plugin) *Base {
	return &Base{
		Plugin:                     plugin,
		CommunicationClientManager: communicate.NewClientManager(),
	}
}

func (s *Base) Context() context.Context {
	return s.ctx
}

func (s *Base) Init(req *v1.InitRequest, settings any) error {

	s.Identity = req.Identity
	s.ServiceLogger = plugins.NewServiceLogger(s.Identity.Name)

	pluginName := fmt.Sprintf("%s::%s", s.Identity.Name, s.Plugin.Name())
	s.PluginLogger = plugins.NewPluginLogger(pluginName)
	defer s.PluginLogger.Catch()
	s.Location = req.Location

	s.ConfigurationLocation = path.Join(s.Location, "codefly")
	err := shared.CheckDirectoryOrCreate(s.ConfigurationLocation)

	if err != nil {
		return s.Wrapf(err, "cannot create configuration directory")
	}

	s.PluginLogger.Debugf("Location %v", s.Location)
	if req.Debug {
		s.PluginLogger.SetDebug() // For developers
	}

	ctx := context.Background()
	ctx = context.WithValue(ctx, shared.Plugin, s.PluginLogger)
	ctx = context.WithValue(ctx, shared.Service, s.ServiceLogger)
	s.ctx = ctx

	s.Configuration, err = configurations.LoadFromDir[configurations.Service](s.Location)
	if err != nil {
		return s.Wrapf(err, "cannot load service configuration")
	}

	err = s.Configuration.LoadSettingsFromSpec(settings)
	if err != nil {
		return s.Wrapf(err, "cannot load settings from spec")
	}
	s.CommunicationClientManager.WithLogger(s.PluginLogger)
	return nil
}

func (s *Base) Create(settings any, endpoints ...*corev1.Endpoint) (*factoryv1.CreateResponse, error) {
	err := s.Configuration.UpdateSpecFromSettings(settings)
	if err != nil {
		return nil, s.Wrapf(err, "cannot update spec")
	}
	err = s.Configuration.Save()
	if err != nil {
		return nil, s.Wrapf(err, "cannot save configuration")
	}
	return &factoryv1.CreateResponse{
		Endpoints: endpoints,
	}, nil
}

func (s *Base) RuntimeInitResponse(endpoints []*corev1.Endpoint, channels ...*corev1.Channel) (*runtimev1.InitResponse, error) {
	// for convenience, add application and service
	for _, endpoint := range endpoints {
		endpoint.Application = s.Configuration.Application
		endpoint.Service = s.Configuration.Name
	}
	return &runtimev1.InitResponse{
		Version:   s.Version(),
		Endpoints: endpoints,
		Channels:  channels,
		Status:    &runtimev1.InitStatus{State: runtimev1.InitStatus_READY},
	}, nil
}

func (s *Base) RuntimeInitResponseError(err error) (*runtimev1.InitResponse, error) {
	return &runtimev1.InitResponse{
		Status: &runtimev1.InitStatus{State: runtimev1.InitStatus_ERROR, Message: err.Error()},
	}, nil
}

/* Some very important helpers */

func (s *Base) Wrapf(err error, format string, args ...interface{}) error {
	return s.PluginLogger.Wrapf(err, format, args...)
}

// EndpointsFromConfiguration from Configuration and data from the service
func (s *Base) EndpointsFromConfiguration() ([]*corev1.Endpoint, error) {
	var eps []*corev1.Endpoint
	for _, e := range s.Configuration.Endpoints {
		if e.Api == configurations.Grpc {
			endpoint, err := endpoints.NewGrpcApi(e, s.Local("api.proto"))
			if err != nil {
				return nil, s.PluginLogger.Wrapf(err, "cannot create grpc api")
			}
			eps = append(eps, endpoint)
			continue
		}
		if e.Api == configurations.Rest {
			endpoint, err := endpoints.NewRestApiFromOpenAPI(s.Context(), e, s.Local("api.swagger.json"))
			if err != nil {
				return nil, s.PluginLogger.Wrapf(err, "cannot create grpc api")
			}
			eps = append(eps, endpoint)
			continue
		}
	}
	return eps, nil
}

type WatchConfiguration struct {
	Includes []string
	Excludes []string
}

func NewWatchConfiguration(includes []string, excludes ...string) *WatchConfiguration {
	return &WatchConfiguration{
		Includes: includes,
		Excludes: excludes,
	}
}

func (s *Base) SetupWatcher(conf *WatchConfiguration, handler func(event code.Change) error) error {
	s.PluginLogger.Debugf("watching for changes")
	s.Events = make(chan code.Change)
	var err error
	s.Watcher, err = code.NewWatcher(s.PluginLogger, s.Events, s.Location, conf.Includes, conf.Excludes...)
	if err != nil {
		return err
	}
	go s.Watcher.Start()

	go func() {
		for event := range s.Events {
			err := handler(event)
			if err != nil {
				s.PluginLogger.Debugf("OOPS: %v", err)
			}
		}
	}()
	return nil
}

func (s *Base) Local(f string) string {
	return path.Join(s.Location, f)
}

/* Helpers

 */

func (s *Base) DebugMe(format string, args ...any) {
	s.PluginLogger.DebugMe(format, args...)
}

func ConfigureError(err error) *runtimev1.ConfigureStatus {
	return &runtimev1.ConfigureStatus{
		State:   runtimev1.ConfigureStatus_ERROR,
		Message: err.Error(),
	}
}

func ConfigureSuccess() *runtimev1.ConfigureStatus {
	return &runtimev1.ConfigureStatus{
		State: runtimev1.ConfigureStatus_READY,
	}
}

func StartError(err error) *runtimev1.StartStatus {
	return &runtimev1.StartStatus{
		State:   runtimev1.StartStatus_ERROR,
		Message: err.Error(),
	}
}

func StartSuccess() *runtimev1.StartStatus {
	return &runtimev1.StartStatus{
		State: runtimev1.StartStatus_STARTED,
	}
}

func (s *Base) Version() *v1.Version {
	return &v1.Version{Version: s.Configuration.Version}
}

func (s *Base) WantRestart() {
	s.Status = RestartWanted
}

func (s *Base) WantSync() {
	s.Status = SyncWanted
}

func (s *Base) Stop() error {
	s.Status = Stopped
	close(s.Events)
	return nil
}

type Channel struct {
	Method corev1.Method
	Client *communicate.ClientContext
}

func NewChannel(method corev1.Method, client *communicate.ClientContext) *Channel {
	return &Channel{Method: method, Client: client}
}

func NewDynamicChannel(method corev1.Method) *Channel {
	return &Channel{Method: method}
}

func (s *Base) WithCommunications(channels ...*Channel) ([]*corev1.Channel, error) {
	var out []*corev1.Channel
	for _, c := range channels {
		out = append(out, &corev1.Channel{Method: c.Method})
		if c.Client == nil {
			continue
		}
		err := s.CommunicationClientManager.Add(c.Method, c.Client)
		if err != nil {
			return nil, s.PluginLogger.Wrapf(err, "cannot add communication client")
		}
	}
	return out, nil
}

func (s *Base) Wire(method corev1.Method, client *communicate.ClientContext) error {
	return s.CommunicationClientManager.Add(method, client)
}

func (s *Base) Communicate(eng *corev1.Engage) (*corev1.InformationRequest, error) {
	if eng.Method == corev1.Method_UNKNOWN {
		return nil, s.PluginLogger.Errorf("unknown method")
	}
	s.PluginLogger.DebugMe("SENDING TO CLIENT MANAGER: %v", eng)
	return s.CommunicationClientManager.Process(eng)
}

type TemplateWrapper struct {
	dir      shared.Dir
	fs       *shared.FSReader
	relative string
	ignores  []string
}

func WithFactory(fs embed.FS, ignores ...string) TemplateWrapper {
	return TemplateWrapper{fs: shared.Embed(fs), dir: shared.NewDir("templates/factory"), ignores: ignores}
}

func WithBuilder(fs embed.FS) TemplateWrapper {
	return TemplateWrapper{fs: shared.Embed(fs), dir: shared.NewDir("templates/builder"), relative: "codefly/builder"}
}

func WithDeployment(fs embed.FS) TemplateWrapper {
	return TemplateWrapper{fs: shared.Embed(fs), dir: shared.NewDir("templates/deployment"), relative: "codefly/deployment"}
}

func WithDeploymentFor(fs embed.FS, relativePath string) TemplateWrapper {
	return TemplateWrapper{fs: shared.Embed(fs),
		dir:      shared.NewDir("templates/deployment/%s", relativePath),
		relative: fmt.Sprintf("codefly/deployment/%s", relativePath)}
}

func (s *Base) Templates(obj any, ws ...TemplateWrapper) error {
	s.PluginLogger.Debugf("templates: %v", s.Location)
	for _, w := range ws {
		ignore := templates.NewIgnore(w.ignores...)
		err := templates.CopyAndApply(s.PluginLogger, w.fs, w.dir, shared.NewDir(s.Local(w.relative)), obj, ignore)
		if err != nil {
			return s.PluginLogger.Wrapf(err, "cannot copy and apply template")
		}
	}
	return nil
}
