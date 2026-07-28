package localservice

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *manager) managerName() string {
	if m.platform == "darwin" {
		return "launchd"
	}
	if m.platform == "linux" {
		return "systemd-user"
	}
	return m.platform
}

func (m *manager) launchdDomain() string {
	return fmt.Sprintf("gui/%d", m.uid)
}

func (m *manager) launchdTarget(ref ServiceRef) string {
	return m.launchdDomain() + "/" + ref.Label
}

func (m *manager) systemdUnit(ref ServiceRef) string {
	return ref.Label + ".service"
}

func (m *manager) prepareUpdate(ctx context.Context, ref ServiceRef) (bool, error) {
	switch m.platform {
	case "darwin":
		_, err := m.run(ctx, "launchctl", "print", m.launchdTarget(ref))
		if err == nil {
			return true, nil
		}
		if commandReportsMissing(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect existing LaunchAgent: %w", err)
	case "linux":
		output, err := m.run(ctx, "systemctl", "--user", "show",
			"--property=LoadState,ActiveState", m.systemdUnit(ref))
		if err != nil && !strings.Contains(output, "LoadState=") {
			return false, fmt.Errorf("inspect existing systemd user unit: %w", err)
		}
		fields := parseKeyValueOutput(output)
		return fields["ActiveState"] == "active" || fields["ActiveState"] == "activating", nil
	default:
		return false, unsupportedPlatform(m.platform)
	}
}

func (m *manager) orphanedServiceStatus(ctx context.Context, ref ServiceRef, path string) (ServiceStatus, error) {
	status := m.notInstalledStatus(ref, path)
	switch m.platform {
	case "darwin":
		output, err := m.run(ctx, "launchctl", "print", m.launchdTarget(ref))
		if err != nil {
			if commandReportsMissing(err) {
				return status, nil
			}
			return ServiceStatus{}, fmt.Errorf("inspect orphaned LaunchAgent: %w", err)
		}
		fields := parseLaunchdOutput(output)
		status.State = ServiceStaleCorrupt
		status.Installed = true
		status.Diagnostics.NativeState = fields["state"]
		status.Diagnostics.PID, _ = strconv.Atoi(fields["pid"])
		status.Diagnostics.ExitCode = parseExitCode(fields["last exit code"])
		if status.Diagnostics.ExitCode == nil {
			status.Diagnostics.ExitCode = parseExitCode(fields["last exit status"])
		}
		status.Diagnostics.ExitReason = fields["last exit reason"]
		status.Diagnostics.RestartCount, _ = strconv.Atoi(fields["runs"])
		status.Diagnostics.Message = "launchd has loaded this service without its Codefly definition"
		status.Running = status.Diagnostics.PID > 0
		return status, nil
	case "linux":
		output, err := m.run(ctx, "systemctl", "--user", "show",
			"--property=LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ExecMainCode,NRestarts,Result",
			m.systemdUnit(ref))
		if err != nil && !strings.Contains(output, "LoadState=") {
			if commandReportsMissing(err) {
				return status, nil
			}
			return ServiceStatus{}, fmt.Errorf("inspect orphaned systemd user unit: %w", err)
		}
		fields := parseKeyValueOutput(output)
		if (fields["LoadState"] == "" || fields["LoadState"] == "not-found") &&
			(fields["ActiveState"] == "" || fields["ActiveState"] == "inactive") {
			return status, nil
		}
		status.State = ServiceStaleCorrupt
		status.Installed = true
		status.Diagnostics.NativeState = fields["ActiveState"]
		if fields["SubState"] != "" {
			status.Diagnostics.NativeState += "/" + fields["SubState"]
		}
		status.Diagnostics.PID, _ = strconv.Atoi(fields["MainPID"])
		status.Diagnostics.ExitCode = parseExitCode(fields["ExecMainStatus"])
		status.Diagnostics.ExitReason = fields["Result"]
		status.Diagnostics.RestartCount, _ = strconv.Atoi(fields["NRestarts"])
		status.Diagnostics.Message = "systemd has loaded this service without its Codefly definition"
		status.Running = status.Diagnostics.PID > 0
		return status, nil
	default:
		return ServiceStatus{}, unsupportedPlatform(m.platform)
	}
}

func (m *manager) applyInstallation(ctx context.Context, request InstallServiceRequest, path string, wasLoaded bool) error {
	switch m.platform {
	case "darwin":
		if wasLoaded {
			if _, err := m.run(ctx, "launchctl", "bootout", m.launchdTarget(request.Ref)); err != nil && !commandReportsMissing(err) {
				return fmt.Errorf("boot out previous LaunchAgent: %w", err)
			}
			if _, err := m.run(ctx, "launchctl", "enable", m.launchdTarget(request.Ref)); err != nil {
				return fmt.Errorf("enable updated LaunchAgent: %w", err)
			}
			if _, err := m.run(ctx, "launchctl", "bootstrap", m.launchdDomain(), path); err != nil {
				return fmt.Errorf("bootstrap updated LaunchAgent: %w", err)
			}
		}
		return m.applyLoginPolicy(ctx, request)
	case "linux":
		if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd user manager: %w", err)
		}
		if err := m.applyLoginPolicy(ctx, request); err != nil {
			return err
		}
		if wasLoaded {
			if _, err := m.run(ctx, "systemctl", "--user", "try-restart", m.systemdUnit(request.Ref)); err != nil {
				return fmt.Errorf("restart updated systemd user unit: %w", err)
			}
		}
		return nil
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) rollbackInstallation(ctx context.Context, ref ServiceRef, path string, previous []byte, wasLoaded bool) error {
	if previous == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := atomicWrite(path, previous, 0o600); err != nil {
		return err
	}

	switch m.platform {
	case "darwin":
		if wasLoaded {
			if _, err := m.run(ctx, "launchctl", "bootout", m.launchdTarget(ref)); err != nil && !commandReportsMissing(err) {
				return err
			}
		}
		if previous == nil {
			_, err := m.run(ctx, "launchctl", "enable", m.launchdTarget(ref))
			return err
		}
		oldRequest, err := contractFromDefinition(previous)
		if err != nil {
			return err
		}
		if wasLoaded {
			if _, err := m.run(ctx, "launchctl", "enable", m.launchdTarget(ref)); err != nil {
				return err
			}
			if _, err := m.run(ctx, "launchctl", "bootstrap", m.launchdDomain(), path); err != nil {
				return err
			}
		}
		return m.applyLoginPolicy(ctx, oldRequest)
	case "linux":
		if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if previous == nil {
			_, err := m.run(ctx, "systemctl", "--user", "disable", m.systemdUnit(ref))
			return err
		}
		oldRequest, err := contractFromDefinition(previous)
		if err != nil {
			return err
		}
		if err := m.applyLoginPolicy(ctx, oldRequest); err != nil {
			return err
		}
		if wasLoaded {
			_, err = m.run(ctx, "systemctl", "--user", "try-restart", m.systemdUnit(ref))
		}
		return err
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) start(ctx context.Context, request InstallServiceRequest, path string) error {
	switch m.platform {
	case "darwin":
		if _, err := m.run(ctx, "launchctl", "enable", m.launchdTarget(request.Ref)); err != nil {
			return fmt.Errorf("enable LaunchAgent: %w", err)
		}
		if _, err := m.run(ctx, "launchctl", "print", m.launchdTarget(request.Ref)); err != nil {
			if !commandReportsMissing(err) {
				return m.restoreLoginPolicy(ctx, request, fmt.Errorf("inspect LaunchAgent before start: %w", err))
			}
			if _, err := m.run(ctx, "launchctl", "bootstrap", m.launchdDomain(), path); err != nil {
				return m.restoreLoginPolicy(ctx, request, fmt.Errorf("bootstrap LaunchAgent: %w", err))
			}
		}
		if _, err := m.run(ctx, "launchctl", "kickstart", m.launchdTarget(request.Ref)); err != nil {
			return m.restoreLoginPolicy(ctx, request, fmt.Errorf("start LaunchAgent: %w", err))
		}
		if err := m.clearOperatorStopped(request.Ref); err != nil {
			return err
		}
		return m.applyLoginPolicy(ctx, request)
	case "linux":
		if err := m.resetSystemdFailure(ctx, request.Ref); err != nil {
			return err
		}
		if _, err := m.run(ctx, "systemctl", "--user", "start", m.systemdUnit(request.Ref)); err != nil {
			return fmt.Errorf("start systemd user unit: %w", err)
		}
		if err := m.clearOperatorStopped(request.Ref); err != nil {
			return err
		}
		return m.applyLoginPolicy(ctx, request)
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) stop(ctx context.Context, ref ServiceRef) error {
	if err := m.markOperatorStopped(ref); err != nil {
		return err
	}
	switch m.platform {
	case "darwin":
		if _, err := m.run(ctx, "launchctl", "disable", m.launchdTarget(ref)); err != nil {
			return fmt.Errorf("disable LaunchAgent: %w", err)
		}
		if _, err := m.run(ctx, "launchctl", "bootout", m.launchdTarget(ref)); err != nil && !commandReportsMissing(err) {
			return fmt.Errorf("stop LaunchAgent: %w", err)
		}
		return nil
	case "linux":
		if _, err := m.run(ctx, "systemctl", "--user", "stop", m.systemdUnit(ref)); err != nil {
			return fmt.Errorf("stop systemd user unit: %w", err)
		}
		if _, err := m.run(ctx, "systemctl", "--user", "disable", m.systemdUnit(ref)); err != nil {
			return fmt.Errorf("disable stopped systemd user unit: %w", err)
		}
		return nil
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) restart(ctx context.Context, request InstallServiceRequest, path string) error {
	switch m.platform {
	case "darwin":
		if _, err := m.run(ctx, "launchctl", "enable", m.launchdTarget(request.Ref)); err != nil {
			return fmt.Errorf("enable LaunchAgent: %w", err)
		}
		if _, err := m.run(ctx, "launchctl", "print", m.launchdTarget(request.Ref)); err != nil {
			if !commandReportsMissing(err) {
				return m.restoreLoginPolicy(ctx, request, fmt.Errorf("inspect LaunchAgent before restart: %w", err))
			}
			if _, err := m.run(ctx, "launchctl", "bootstrap", m.launchdDomain(), path); err != nil {
				return m.restoreLoginPolicy(ctx, request, fmt.Errorf("bootstrap LaunchAgent: %w", err))
			}
		}
		if _, err := m.run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget(request.Ref)); err != nil {
			return m.restoreLoginPolicy(ctx, request, fmt.Errorf("restart LaunchAgent: %w", err))
		}
		if err := m.clearOperatorStopped(request.Ref); err != nil {
			return err
		}
		return m.applyLoginPolicy(ctx, request)
	case "linux":
		if err := m.resetSystemdFailure(ctx, request.Ref); err != nil {
			return err
		}
		if _, err := m.run(ctx, "systemctl", "--user", "restart", m.systemdUnit(request.Ref)); err != nil {
			return fmt.Errorf("restart systemd user unit: %w", err)
		}
		if err := m.clearOperatorStopped(request.Ref); err != nil {
			return err
		}
		return m.applyLoginPolicy(ctx, request)
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) removeInstallation(ctx context.Context, ref ServiceRef) error {
	switch m.platform {
	case "darwin":
		if _, err := m.run(ctx, "launchctl", "bootout", m.launchdTarget(ref)); err != nil && !commandReportsMissing(err) {
			return fmt.Errorf("boot out LaunchAgent: %w", err)
		}
		if _, err := m.run(ctx, "launchctl", "enable", m.launchdTarget(ref)); err != nil {
			return fmt.Errorf("clear LaunchAgent disable override: %w", err)
		}
		return nil
	case "linux":
		if _, err := m.run(ctx, "systemctl", "--user", "stop", m.systemdUnit(ref)); err != nil && !commandReportsMissing(err) {
			return fmt.Errorf("stop systemd user unit: %w", err)
		}
		if _, err := m.run(ctx, "systemctl", "--user", "disable", m.systemdUnit(ref)); err != nil && !commandReportsMissing(err) {
			return fmt.Errorf("disable systemd user unit: %w", err)
		}
		return nil
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) reloadAfterRemoval(ctx context.Context, ref ServiceRef) error {
	if m.platform != "linux" {
		return nil
	}
	if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd user manager: %w", err)
	}
	if err := m.resetSystemdFailure(ctx, ref); err != nil {
		return err
	}
	return nil
}

func (m *manager) resetSystemdFailure(ctx context.Context, ref ServiceRef) error {
	if _, err := m.run(ctx, "systemctl", "--user", "reset-failed", m.systemdUnit(ref)); err != nil && !commandReportsMissing(err) {
		return fmt.Errorf("reset failed systemd user unit: %w", err)
	}
	return nil
}

func (m *manager) nativeStatus(ctx context.Context, request InstallServiceRequest, path string) (ServiceStatus, error) {
	base := ServiceStatus{
		Ref:          request.Ref,
		Version:      request.Version,
		State:        ServiceInstalledStopped,
		Installed:    true,
		StartAtLogin: request.StartAtLogin,
		Diagnostics: ServiceDiagnostic{
			Manager:        m.managerName(),
			DefinitionPath: path,
		},
	}
	switch m.platform {
	case "darwin":
		return m.launchdStatus(ctx, request, base)
	case "linux":
		return m.systemdStatus(ctx, request, base)
	default:
		return ServiceStatus{}, unsupportedPlatform(m.platform)
	}
}

func (m *manager) setLaunchdLoginPolicy(ctx context.Context, request InstallServiceRequest, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	if _, err := m.run(ctx, "launchctl", action, m.launchdTarget(request.Ref)); err != nil {
		return fmt.Errorf("%s LaunchAgent login policy: %w", action, err)
	}
	return nil
}

func (m *manager) applyLoginPolicy(ctx context.Context, request InstallServiceRequest) error {
	operatorStopped, err := m.operatorStopped(request.Ref)
	if err != nil {
		return err
	}
	enabled := request.StartAtLogin && !operatorStopped
	switch m.platform {
	case "darwin":
		return m.setLaunchdLoginPolicy(ctx, request, enabled)
	case "linux":
		action := "disable"
		if enabled {
			action = "enable"
		}
		if _, err := m.run(ctx, "systemctl", "--user", action, m.systemdUnit(request.Ref)); err != nil {
			return fmt.Errorf("%s systemd user unit: %w", action, err)
		}
		return nil
	default:
		return unsupportedPlatform(m.platform)
	}
}

func (m *manager) restoreLoginPolicy(ctx context.Context, request InstallServiceRequest, operationErr error) error {
	if err := m.applyLoginPolicy(ctx, request); err != nil {
		return fmt.Errorf("%w (restore login policy: %v)", operationErr, err)
	}
	return operationErr
}

func (m *manager) launchdStatus(ctx context.Context, request InstallServiceRequest, status ServiceStatus) (ServiceStatus, error) {
	output, err := m.run(ctx, "launchctl", "print", m.launchdTarget(request.Ref))
	if err != nil {
		if commandReportsMissing(err) {
			return status, nil
		}
		return ServiceStatus{}, fmt.Errorf("inspect LaunchAgent: %w", err)
	}
	fields := parseLaunchdOutput(output)
	status.Diagnostics.NativeState = fields["state"]
	status.Diagnostics.PID, _ = strconv.Atoi(fields["pid"])
	status.Diagnostics.RestartCount, _ = strconv.Atoi(fields["runs"])
	status.Diagnostics.ExitCode = parseExitCode(fields["last exit code"])
	if status.Diagnostics.ExitCode == nil {
		status.Diagnostics.ExitCode = parseExitCode(fields["last exit status"])
	}
	status.Diagnostics.ExitReason = fields["last exit reason"]
	if status.Diagnostics.PID > 0 {
		status.Running = true
		status.State = ServiceRunningUnhealthy
		return status, nil
	}
	switch fields["state"] {
	case "active", "running", "spawn scheduled", "waiting":
		if nonZero(status.Diagnostics.ExitCode) && status.Diagnostics.RestartCount >= 5 {
			status.State = ServiceCrashLooping
		} else {
			status.State = ServiceStarting
		}
	case "failed":
		if status.Diagnostics.RestartCount >= 5 {
			status.State = ServiceCrashLooping
		} else {
			status.State = ServiceFailed
		}
	default:
		if nonZero(status.Diagnostics.ExitCode) {
			status.State = ServiceFailed
		}
	}
	return status, nil
}

func (m *manager) systemdStatus(ctx context.Context, request InstallServiceRequest, status ServiceStatus) (ServiceStatus, error) {
	output, err := m.run(ctx, "systemctl", "--user", "show",
		"--property=LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ExecMainCode,NRestarts,Result",
		m.systemdUnit(request.Ref))
	if err != nil && !strings.Contains(output, "LoadState=") {
		return ServiceStatus{}, fmt.Errorf("inspect systemd user unit: %w", err)
	}
	fields := parseKeyValueOutput(output)
	if fields["LoadState"] != "loaded" {
		status.State = ServiceStaleCorrupt
		status.Diagnostics.NativeState = fields["LoadState"]
		status.Diagnostics.Message = "systemd user manager has not loaded the installed definition"
		return status, nil
	}
	status.Diagnostics.NativeState = fields["ActiveState"]
	if fields["SubState"] != "" {
		status.Diagnostics.NativeState += "/" + fields["SubState"]
	}
	status.Diagnostics.PID, _ = strconv.Atoi(fields["MainPID"])
	status.Diagnostics.ExitCode = parseExitCode(fields["ExecMainStatus"])
	status.Diagnostics.ExitReason = fields["Result"]
	if code := fields["ExecMainCode"]; code != "" && code != "0" {
		if status.Diagnostics.ExitReason == "" {
			status.Diagnostics.ExitReason = code
		} else {
			status.Diagnostics.ExitReason += " (" + code + ")"
		}
	}
	status.Diagnostics.RestartCount, _ = strconv.Atoi(fields["NRestarts"])
	switch fields["ActiveState"] {
	case "active":
		status.Running = status.Diagnostics.PID > 0
		if status.Running {
			status.State = ServiceRunningUnhealthy
		} else {
			status.State = ServiceStarting
		}
	case "activating", "reloading":
		status.State = ServiceStarting
	case "failed":
		if status.Diagnostics.RestartCount >= 5 || fields["Result"] == "start-limit-hit" {
			status.State = ServiceCrashLooping
		} else {
			status.State = ServiceFailed
		}
	case "inactive", "deactivating", "":
		status.State = ServiceInstalledStopped
	default:
		status.State = ServiceFailed
		status.Diagnostics.Message = "unrecognized systemd active state"
	}
	return status, nil
}

func parseLaunchdOutput(output string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		switch key {
		case "state", "pid", "runs", "last exit code", "last exit status", "last exit reason":
			fields[key] = strings.TrimSpace(value)
		}
	}
	return fields
}

func parseKeyValueOutput(output string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields
}

func nonZero(value *int) bool {
	return value != nil && *value != 0
}

func commandReportsMissing(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unit file") && strings.Contains(message, "does not exist") {
		return true
	}
	for _, fragment := range []string{
		"could not find service",
		"service not found",
		"not loaded",
		"no such process",
		"failed to get unit",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func (m *manager) notInstalledStatus(ref ServiceRef, path string) ServiceStatus {
	return ServiceStatus{
		Ref:   ref,
		State: ServiceNotInstalled,
		Diagnostics: ServiceDiagnostic{
			Manager:        m.managerName(),
			DefinitionPath: path,
		},
	}
}

func (m *manager) recentLogs(ctx context.Context, request InstallServiceRequest) []string {
	if request.Logs.Mode == LogNative && m.platform == "linux" {
		output, err := m.run(ctx, "journalctl", "--user", "-u", m.systemdUnit(request.Ref),
			"-n", "20", "--no-pager", "--output=cat")
		if err != nil || output == "" {
			return nil
		}
		return strings.Split(output, "\n")
	}
	var lines []string
	for _, path := range request.Logs.paths() {
		for _, line := range tailLines(path, 10) {
			lines = append(lines, filepath.Base(path)+": "+line)
		}
	}
	return lines
}

func tailLines(path string, limit int) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	return lines
}
