// Package localservice manages durable per-user services through the host
// operating system's native service manager.
package localservice

import (
	"context"
	"time"
)

// Installation is the platform-neutral lifecycle for a durable local service.
// Implementations use launchd LaunchAgents on macOS and systemd user units on
// Linux. The managed process must remain in the foreground.
type Installation interface {
	InstallService(context.Context, InstallServiceRequest) (InstalledService, error)
	StartService(context.Context, ServiceRef) (ServiceStatus, error)
	StopService(context.Context, ServiceRef) (ServiceStatus, error)
	RestartService(context.Context, ServiceRef) (ServiceStatus, error)
	UninstallService(context.Context, UninstallServiceRequest) error
	ServiceStatus(context.Context, ServiceRef) (ServiceStatus, error)
}

// ServiceRef identifies one stable per-user service definition.
type ServiceRef struct {
	Label string `json:"label"`
}

// InstallServiceRequest is the complete materialized supervisor contract.
// Version must change whenever any other field changes.
type InstallServiceRequest struct {
	Ref              ServiceRef            `json:"ref"`
	Version          string                `json:"version"`
	Executable       string                `json:"executable"`
	Arguments        []ServiceArgument     `json:"arguments,omitempty"`
	Environment      []EnvironmentVariable `json:"environment,omitempty"`
	WorkingDirectory string                `json:"working_directory,omitempty"`
	Health           HealthProbe           `json:"health,omitempty"`
	Restart          RestartPolicy         `json:"restart"`
	RestartDelay     time.Duration         `json:"restart_delay,omitempty"`
	StartAtLogin     bool                  `json:"start_at_login"`
	Logs             LogRouting            `json:"logs,omitempty"`
}

// ValueClassification requires callers to classify every materialized value.
type ValueClassification string

const (
	ValuePublic    ValueClassification = "public"
	ValueSensitive ValueClassification = "sensitive"
)

// ServiceArgument is one explicitly classified executable argument.
type ServiceArgument struct {
	Value          string              `json:"value"`
	Classification ValueClassification `json:"classification"`
}

// EnvironmentVariable is one explicitly classified environment literal.
type EnvironmentVariable struct {
	Name           string              `json:"name"`
	Value          string              `json:"value,omitempty"`
	Classification ValueClassification `json:"classification"`
}

// RestartPolicy controls native supervisor restart behavior.
type RestartPolicy string

const (
	RestartNever     RestartPolicy = "never"
	RestartOnFailure RestartPolicy = "on-failure"
)

// HealthProbeKind selects the readiness boundary used for service status.
type HealthProbeKind string

const (
	HealthProbeNone HealthProbeKind = ""
	HealthProbeHTTP HealthProbeKind = "http"
	HealthProbeTCP  HealthProbeKind = "tcp"
)

// HealthProbe describes a product readiness check. Timeout is the total wait
// allowed by start and restart; Interval controls retries during that wait.
type HealthProbe struct {
	Kind     HealthProbeKind `json:"kind,omitempty"`
	Target   string          `json:"target,omitempty"`
	Timeout  time.Duration   `json:"timeout,omitempty"`
	Interval time.Duration   `json:"interval,omitempty"`
}

// LogMode selects OS-native logging or explicit user-owned files.
type LogMode string

const (
	LogNative LogMode = "native"
	LogFiles  LogMode = "files"
)

// LogRouting controls stdout and stderr routing.
type LogRouting struct {
	Mode       LogMode `json:"mode,omitempty"`
	StdoutPath string  `json:"stdout_path,omitempty"`
	StderrPath string  `json:"stderr_path,omitempty"`
}

// InstalledService describes the definition written by InstallService.
type InstalledService struct {
	Ref            ServiceRef `json:"ref"`
	Version        string     `json:"version"`
	DefinitionPath string     `json:"definition_path"`
	StartAtLogin   bool       `json:"start_at_login"`
	Updated        bool       `json:"updated"`
}

// UninstallServiceRequest removes supervisor configuration only. Product data,
// credentials, and logs are deliberately outside this operation.
type UninstallServiceRequest struct {
	Ref     ServiceRef `json:"ref"`
	Version string     `json:"version,omitempty"`
}

// ServiceState is the durable state observed from the native supervisor and
// the product health boundary.
type ServiceState string

const (
	ServiceNotInstalled     ServiceState = "not-installed"
	ServiceInstalledStopped ServiceState = "installed-stopped"
	ServiceStarting         ServiceState = "starting"
	ServiceRunningHealthy   ServiceState = "running-healthy"
	ServiceRunningUnhealthy ServiceState = "running-unhealthy"
	ServiceCrashLooping     ServiceState = "crash-looping"
	ServiceFailed           ServiceState = "failed"
	ServiceStaleCorrupt     ServiceState = "stale-corrupt"
)

// ServiceStatus combines native process state with product readiness.
type ServiceStatus struct {
	Ref             ServiceRef        `json:"ref"`
	Version         string            `json:"version,omitempty"`
	State           ServiceState      `json:"state"`
	Installed       bool              `json:"installed"`
	Running         bool              `json:"running"`
	Healthy         bool              `json:"healthy"`
	StartAtLogin    bool              `json:"start_at_login"`
	OperatorStopped bool              `json:"operator_stopped"`
	Diagnostics     ServiceDiagnostic `json:"diagnostics"`
}

// ServiceDiagnostic exposes parsed native details so callers never need to
// interpret launchctl, systemctl, or journal output.
type ServiceDiagnostic struct {
	Manager        string   `json:"manager"`
	DefinitionPath string   `json:"definition_path,omitempty"`
	NativeState    string   `json:"native_state,omitempty"`
	PID            int      `json:"pid,omitempty"`
	ExitCode       *int     `json:"exit_code,omitempty"`
	ExitReason     string   `json:"exit_reason,omitempty"`
	RestartCount   int      `json:"restart_count,omitempty"`
	Message        string   `json:"message,omitempty"`
	LogPaths       []string `json:"log_paths,omitempty"`
	RecentLogs     []string `json:"recent_logs,omitempty"`
}
