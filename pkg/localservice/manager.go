package localservice

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	labelPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type manager struct {
	platform string
	home     string
	uid      int
	run      commandRunner
}

type commandRunner func(context.Context, string, ...string) (string, error)

type commandError struct {
	command []string
	output  string
	err     error
}

func (e *commandError) Error() string {
	if e.output == "" {
		return fmt.Sprintf("%s failed: %v", strings.Join(e.command, " "), e.err)
	}
	return fmt.Sprintf("%s failed: %s", strings.Join(e.command, " "), e.output)
}

func (e *commandError) Unwrap() error {
	return e.err
}

// New returns the native local-service lifecycle for the current user.
func New() (Installation, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, unsupportedPlatform(runtime.GOOS)
	}
	return newManager(runtime.GOOS, home, os.Getuid(), executeCommand), nil
}

func newManager(platform, home string, uid int, run commandRunner) *manager {
	return &manager{platform: platform, home: home, uid: uid, run: run}
}

func executeCommand(ctx context.Context, name string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if err == nil {
		return trimmed, nil
	}
	return trimmed, &commandError{
		command: append([]string{name}, arguments...),
		output:  trimmed,
		err:     err,
	}
}

func unsupportedPlatform(platform string) error {
	return fmt.Errorf("local services are unsupported on %s; supported platforms are macOS and Linux", platform)
}

func (m *manager) InstallService(ctx context.Context, request InstallServiceRequest) (InstalledService, error) {
	request = m.normalizeRequest(request)
	definition, err := renderDefinition(m.platform, request)
	if err != nil {
		return InstalledService{}, err
	}
	path, err := m.definitionPath(request.Ref)
	if err != nil {
		return InstalledService{}, err
	}

	var previous []byte
	previous, err = os.ReadFile(path)
	switch {
	case err == nil:
		if safetyErr := validateDefinitionFile(path, m.uid); safetyErr != nil {
			return InstalledService{}, fmt.Errorf("refuse to replace unsafe service %q: %w", request.Ref.Label, safetyErr)
		}
		existing, validationErr := validateDefinition(m.platform, previous)
		if validationErr != nil {
			return InstalledService{}, fmt.Errorf("refuse to replace stale or corrupt service %q: %w", request.Ref.Label, validationErr)
		}
		if existing.Version == request.Version && string(previous) != string(definition) {
			return InstalledService{}, fmt.Errorf("service %q contract changed without a version change", request.Ref.Label)
		}
		if string(previous) == string(definition) {
			if err := m.applyLoginPolicy(ctx, request); err != nil {
				return InstalledService{}, err
			}
			return InstalledService{
				Ref:            request.Ref,
				Version:        request.Version,
				DefinitionPath: path,
				StartAtLogin:   request.StartAtLogin,
				Updated:        false,
			}, nil
		}
	case os.IsNotExist(err):
		previous = nil
	case err != nil:
		return InstalledService{}, fmt.Errorf("read existing service definition: %w", err)
	}

	wasLoaded, err := m.prepareUpdate(ctx, request.Ref, previous != nil)
	if err != nil {
		return InstalledService{}, err
	}
	if err := prepareLogFiles(request.Logs); err != nil {
		return InstalledService{}, err
	}
	if err := atomicWrite(path, definition, 0o600); err != nil {
		return InstalledService{}, fmt.Errorf("write service definition: %w", err)
	}
	if err := m.applyInstallation(ctx, request, path, wasLoaded); err != nil {
		rollbackErr := m.rollbackInstallation(ctx, request.Ref, path, previous, wasLoaded)
		if rollbackErr != nil {
			return InstalledService{}, fmt.Errorf("apply service definition: %w (rollback failed: %v)", err, rollbackErr)
		}
		return InstalledService{}, fmt.Errorf("apply service definition: %w", err)
	}
	return InstalledService{
		Ref:            request.Ref,
		Version:        request.Version,
		DefinitionPath: path,
		StartAtLogin:   request.StartAtLogin,
		Updated:        previous != nil,
	}, nil
}

func (m *manager) StartService(ctx context.Context, ref ServiceRef) (ServiceStatus, error) {
	request, path, err := m.installedRequest(ref)
	if err != nil {
		if os.IsNotExist(err) {
			return m.notInstalledStatus(ref, path), nil
		}
		return ServiceStatus{}, err
	}
	if err := m.start(ctx, request, path); err != nil {
		return ServiceStatus{}, err
	}
	return m.waitForReadiness(ctx, request)
}

func (m *manager) StopService(ctx context.Context, ref ServiceRef) (ServiceStatus, error) {
	request, path, err := m.installedRequest(ref)
	if err != nil {
		if os.IsNotExist(err) {
			return m.notInstalledStatus(ref, path), nil
		}
		return ServiceStatus{}, err
	}
	if err := m.stop(ctx, request.Ref); err != nil {
		return ServiceStatus{}, err
	}
	return m.ServiceStatus(ctx, request.Ref)
}

func (m *manager) RestartService(ctx context.Context, ref ServiceRef) (ServiceStatus, error) {
	request, path, err := m.installedRequest(ref)
	if err != nil {
		if os.IsNotExist(err) {
			return m.notInstalledStatus(ref, path), nil
		}
		return ServiceStatus{}, err
	}
	if err := m.restart(ctx, request, path); err != nil {
		return ServiceStatus{}, err
	}
	return m.waitForReadiness(ctx, request)
}

func (m *manager) UninstallService(ctx context.Context, request UninstallServiceRequest) error {
	path, err := m.definitionPath(request.Ref)
	if err != nil {
		return err
	}
	definition, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read service definition: %w", err)
	}
	if request.Version != "" {
		installed, parseErr := contractFromDefinition(definition)
		if parseErr != nil {
			return fmt.Errorf("cannot verify version of stale or corrupt service %q: %w", request.Ref.Label, parseErr)
		}
		if request.Version != installed.Version {
			return fmt.Errorf("service %q is version %q, not %q", request.Ref.Label, installed.Version, request.Version)
		}
	}
	if err := m.removeInstallation(ctx, request.Ref); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove service definition: %w", err)
	}
	if err := m.reloadAfterRemoval(ctx); err != nil {
		return err
	}
	return nil
}

func (m *manager) ServiceStatus(ctx context.Context, ref ServiceRef) (ServiceStatus, error) {
	path, pathErr := m.definitionPath(ref)
	if pathErr != nil {
		return ServiceStatus{}, pathErr
	}
	definition, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m.notInstalledStatus(ref, path), nil
	}
	if err != nil {
		return ServiceStatus{}, fmt.Errorf("read service definition: %w", err)
	}
	if err := validateDefinitionFile(path, m.uid); err != nil {
		return ServiceStatus{
			Ref:       ref,
			State:     ServiceStaleCorrupt,
			Installed: true,
			Diagnostics: ServiceDiagnostic{
				Manager:        m.managerName(),
				DefinitionPath: path,
				Message:        err.Error(),
			},
		}, nil
	}
	request, err := validateDefinition(m.platform, definition)
	if err != nil {
		return ServiceStatus{
			Ref:       ref,
			State:     ServiceStaleCorrupt,
			Installed: true,
			Diagnostics: ServiceDiagnostic{
				Manager:        m.managerName(),
				DefinitionPath: path,
				Message:        err.Error(),
			},
		}, nil
	}
	status, err := m.nativeStatus(ctx, request, path)
	if err != nil {
		return ServiceStatus{}, err
	}
	if status.Running {
		healthy, healthErr := checkHealth(ctx, request.Health, healthAttemptTimeout(request.Health))
		status.Healthy = healthy
		if healthy {
			status.State = ServiceRunningHealthy
		} else {
			status.State = ServiceRunningUnhealthy
			if healthErr != nil {
				status.Diagnostics.Message = healthErr.Error()
			}
		}
	}
	status.Diagnostics.LogPaths = request.Logs.paths()
	if status.State == ServiceFailed || status.State == ServiceCrashLooping || status.State == ServiceRunningUnhealthy {
		status.Diagnostics.RecentLogs = m.recentLogs(ctx, request)
	}
	return status, nil
}

func (m *manager) waitForReadiness(ctx context.Context, request InstallServiceRequest) (ServiceStatus, error) {
	timeout := request.Health.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	interval := request.Health.Interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		status, err := m.ServiceStatus(ctx, request.Ref)
		if err != nil {
			return ServiceStatus{}, err
		}
		switch status.State {
		case ServiceRunningHealthy, ServiceFailed, ServiceCrashLooping, ServiceStaleCorrupt:
			return status, nil
		}
		select {
		case <-ctx.Done():
			return ServiceStatus{}, ctx.Err()
		case <-deadline.C:
			return status, nil
		case <-ticker.C:
		}
	}
}

func (m *manager) installedRequest(ref ServiceRef) (InstallServiceRequest, string, error) {
	path, err := m.definitionPath(ref)
	if err != nil {
		return InstallServiceRequest{}, "", err
	}
	definition, err := os.ReadFile(path)
	if err != nil {
		return InstallServiceRequest{}, path, err
	}
	if err := validateDefinitionFile(path, m.uid); err != nil {
		return InstallServiceRequest{}, path, fmt.Errorf("service %q definition is unsafe: %w", ref.Label, err)
	}
	request, err := validateDefinition(m.platform, definition)
	if err != nil {
		return InstallServiceRequest{}, path, fmt.Errorf("service %q is stale or corrupt: %w", ref.Label, err)
	}
	return request, path, nil
}

func (m *manager) normalizeRequest(request InstallServiceRequest) InstallServiceRequest {
	request.Arguments = append([]string(nil), request.Arguments...)
	request.Environment = append([]EnvironmentVariable(nil), request.Environment...)
	request.Executable = cleanAbsolute(request.Executable)
	request.WorkingDirectory = cleanAbsolute(request.WorkingDirectory)
	if request.Restart == "" {
		request.Restart = RestartOnFailure
	}
	if request.RestartDelay <= 0 {
		request.RestartDelay = 5 * time.Second
	}
	if request.Health.Kind != HealthProbeNone {
		if request.Health.Timeout <= 0 {
			request.Health.Timeout = 15 * time.Second
		}
		if request.Health.Interval <= 0 {
			request.Health.Interval = 250 * time.Millisecond
		}
	}
	if request.Logs.Mode == "" {
		if m.platform == "darwin" {
			request.Logs.Mode = LogFiles
		} else {
			request.Logs.Mode = LogNative
		}
	}
	if request.Logs.Mode == LogFiles {
		logDirectory := filepath.Join(m.home, ".codefly", "services", "logs")
		if request.Logs.StdoutPath == "" {
			request.Logs.StdoutPath = filepath.Join(logDirectory, request.Ref.Label+".stdout.log")
		}
		if request.Logs.StderrPath == "" {
			request.Logs.StderrPath = filepath.Join(logDirectory, request.Ref.Label+".stderr.log")
		}
		request.Logs.StdoutPath = cleanAbsolute(request.Logs.StdoutPath)
		request.Logs.StderrPath = cleanAbsolute(request.Logs.StderrPath)
	}
	sort.Slice(request.Environment, func(i, j int) bool {
		return request.Environment[i].Name < request.Environment[j].Name
	})
	return request
}

func validateRequest(request InstallServiceRequest) error {
	if !labelPattern.MatchString(request.Ref.Label) {
		return fmt.Errorf("service label %q is invalid", request.Ref.Label)
	}
	if strings.TrimSpace(request.Version) == "" {
		return fmt.Errorf("service installation version is required")
	}
	if err := validateMaterializedString("service installation version", request.Version); err != nil {
		return err
	}
	if !filepath.IsAbs(request.Executable) {
		return fmt.Errorf("service executable must be absolute")
	}
	info, err := os.Stat(request.Executable)
	if err != nil {
		return fmt.Errorf("inspect service executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("service executable %q is not executable", request.Executable)
	}
	if err := validateMaterializedString("service executable", request.Executable); err != nil {
		return err
	}
	for _, argument := range request.Arguments {
		if err := validateMaterializedString("service argument", argument); err != nil {
			return err
		}
	}
	if request.WorkingDirectory != "" {
		if !filepath.IsAbs(request.WorkingDirectory) {
			return fmt.Errorf("service working directory must be absolute")
		}
		info, err := os.Stat(request.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("inspect service working directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("service working directory %q is not a directory", request.WorkingDirectory)
		}
		if err := validateMaterializedString("service working directory", request.WorkingDirectory); err != nil {
			return err
		}
	}
	switch request.Restart {
	case RestartNever, RestartOnFailure:
	default:
		return fmt.Errorf("restart policy %q is invalid", request.Restart)
	}
	if request.RestartDelay < time.Second {
		return fmt.Errorf("restart delay must be at least 1s")
	}
	seen := make(map[string]struct{}, len(request.Environment))
	for _, variable := range request.Environment {
		if !environmentPattern.MatchString(variable.Name) {
			return fmt.Errorf("environment variable name %q is invalid", variable.Name)
		}
		if variable.Sensitive {
			return fmt.Errorf("sensitive environment variable %q cannot be embedded in a service definition", variable.Name)
		}
		if err := validateMaterializedString("environment value "+variable.Name, variable.Value); err != nil {
			return err
		}
		if _, ok := seen[variable.Name]; ok {
			return fmt.Errorf("environment variable %q is repeated", variable.Name)
		}
		seen[variable.Name] = struct{}{}
	}
	switch request.Health.Kind {
	case HealthProbeNone:
		if request.Health.Target != "" {
			return fmt.Errorf("health target requires a probe kind")
		}
	case HealthProbeHTTP:
		target, err := url.ParseRequestURI(request.Health.Target)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return fmt.Errorf("HTTP health target %q is invalid", request.Health.Target)
		}
	case HealthProbeTCP:
		if _, _, err := net.SplitHostPort(request.Health.Target); err != nil {
			return fmt.Errorf("TCP health target %q is invalid: %w", request.Health.Target, err)
		}
	default:
		return fmt.Errorf("health probe kind %q is invalid", request.Health.Kind)
	}
	switch request.Logs.Mode {
	case LogNative:
	case LogFiles:
		if !filepath.IsAbs(request.Logs.StdoutPath) || !filepath.IsAbs(request.Logs.StderrPath) {
			return fmt.Errorf("service log paths must be absolute")
		}
	default:
		return fmt.Errorf("log mode %q is invalid", request.Logs.Mode)
	}
	for _, value := range []struct {
		name  string
		value string
	}{
		{name: "health target", value: request.Health.Target},
		{name: "stdout log path", value: request.Logs.StdoutPath},
		{name: "stderr log path", value: request.Logs.StderrPath},
	} {
		if err := validateMaterializedString(value.name, value.value); err != nil {
			return err
		}
	}
	return nil
}

func validateMaterializedString(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	for _, character := range value {
		if character < 0x20 && character != '\t' && character != '\n' && character != '\r' {
			return fmt.Errorf("%s contains an unsupported control character", name)
		}
	}
	return nil
}

func (m *manager) definitionPath(ref ServiceRef) (string, error) {
	if !labelPattern.MatchString(ref.Label) {
		return "", fmt.Errorf("service label %q is invalid", ref.Label)
	}
	switch m.platform {
	case "darwin":
		return filepath.Join(m.home, "Library", "LaunchAgents", definitionName(m.platform, ref.Label)), nil
	case "linux":
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(m.home, ".config")
		} else if !filepath.IsAbs(configHome) {
			return "", fmt.Errorf("XDG_CONFIG_HOME must be absolute")
		}
		return filepath.Join(configHome, "systemd", "user", definitionName(m.platform, ref.Label)), nil
	default:
		return "", unsupportedPlatform(m.platform)
	}
}

func atomicWrite(path string, content []byte, permission os.FileMode) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() {
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(permission); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func prepareLogFiles(logs LogRouting) error {
	if logs.Mode != LogFiles {
		return nil
	}
	for _, path := range logs.paths() {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create service log directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create service log file: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return fmt.Errorf("secure service log file: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close service log file: %w", err)
		}
	}
	return nil
}

func (logs LogRouting) paths() []string {
	if logs.Mode != LogFiles {
		return nil
	}
	return []string{logs.StdoutPath, logs.StderrPath}
}

func checkHealth(ctx context.Context, probe HealthProbe, timeout time.Duration) (bool, error) {
	if probe.Kind == HealthProbeNone {
		return true, nil
	}
	checkContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch probe.Kind {
	case HealthProbeHTTP:
		request, err := http.NewRequestWithContext(checkContext, http.MethodGet, probe.Target, nil)
		if err != nil {
			return false, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false, fmt.Errorf("HTTP health probe failed: %w", err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return false, fmt.Errorf("HTTP health probe returned %s", response.Status)
		}
		return true, nil
	case HealthProbeTCP:
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(checkContext, "tcp", probe.Target)
		if err != nil {
			return false, fmt.Errorf("TCP health probe failed: %w", err)
		}
		return true, connection.Close()
	default:
		return false, fmt.Errorf("health probe kind %q is invalid", probe.Kind)
	}
}

func healthAttemptTimeout(probe HealthProbe) time.Duration {
	timeout := probe.Timeout
	if timeout <= 0 || timeout > 2*time.Second {
		return 2 * time.Second
	}
	return timeout
}
