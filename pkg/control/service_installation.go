package control

import (
	"context"

	"github.com/codefly-dev/cli/pkg/localservice"
)

type ServiceRef = localservice.ServiceRef
type InstallServiceRequest = localservice.InstallServiceRequest
type ValueClassification = localservice.ValueClassification
type ServiceArgument = localservice.ServiceArgument
type EnvironmentVariable = localservice.EnvironmentVariable
type RestartPolicy = localservice.RestartPolicy
type HealthProbeKind = localservice.HealthProbeKind
type HealthProbe = localservice.HealthProbe
type LogMode = localservice.LogMode
type LogRouting = localservice.LogRouting
type InstalledService = localservice.InstalledService
type UninstallServiceRequest = localservice.UninstallServiceRequest
type InstalledServiceState = localservice.ServiceState
type InstalledServiceStatus = localservice.ServiceStatus
type ServiceDiagnostic = localservice.ServiceDiagnostic

const (
	ValuePublic    = localservice.ValuePublic
	ValueSensitive = localservice.ValueSensitive

	RestartNever     = localservice.RestartNever
	RestartOnFailure = localservice.RestartOnFailure

	HealthProbeNone = localservice.HealthProbeNone
	HealthProbeHTTP = localservice.HealthProbeHTTP
	HealthProbeTCP  = localservice.HealthProbeTCP

	LogNative = localservice.LogNative
	LogFiles  = localservice.LogFiles

	ServiceNotInstalled     = localservice.ServiceNotInstalled
	ServiceInstalledStopped = localservice.ServiceInstalledStopped
	ServiceStarting         = localservice.ServiceStarting
	ServiceRunningHealthy   = localservice.ServiceRunningHealthy
	ServiceRunningUnhealthy = localservice.ServiceRunningUnhealthy
	ServiceCrashLooping     = localservice.ServiceCrashLooping
	ServiceFailed           = localservice.ServiceFailed
	ServiceStaleCorrupt     = localservice.ServiceStaleCorrupt
)

func (p *planeImpl) InstallService(ctx context.Context, request InstallServiceRequest) (InstalledService, error) {
	installation, err := localservice.New()
	if err != nil {
		return InstalledService{}, err
	}
	return installation.InstallService(ctx, request)
}

func (p *planeImpl) StartService(ctx context.Context, ref ServiceRef) (InstalledServiceStatus, error) {
	installation, err := localservice.New()
	if err != nil {
		return InstalledServiceStatus{}, err
	}
	return installation.StartService(ctx, ref)
}

func (p *planeImpl) StopService(ctx context.Context, ref ServiceRef) (InstalledServiceStatus, error) {
	installation, err := localservice.New()
	if err != nil {
		return InstalledServiceStatus{}, err
	}
	return installation.StopService(ctx, ref)
}

func (p *planeImpl) RestartService(ctx context.Context, ref ServiceRef) (InstalledServiceStatus, error) {
	installation, err := localservice.New()
	if err != nil {
		return InstalledServiceStatus{}, err
	}
	return installation.RestartService(ctx, ref)
}

func (p *planeImpl) UninstallService(ctx context.Context, request UninstallServiceRequest) error {
	installation, err := localservice.New()
	if err != nil {
		return err
	}
	return installation.UninstallService(ctx, request)
}

func (p *planeImpl) ServiceStatus(ctx context.Context, ref ServiceRef) (InstalledServiceStatus, error) {
	installation, err := localservice.New()
	if err != nil {
		return InstalledServiceStatus{}, err
	}
	return installation.ServiceStatus(ctx, ref)
}
