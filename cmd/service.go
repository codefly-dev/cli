package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/control"
	"github.com/spf13/cobra"
)

// ServiceCmd manages durable services through the current user's OS supervisor.
var ServiceCmd = newServiceCommand()

type serviceInstallOptions struct {
	version          string
	executable       string
	arguments        []string
	environment      []string
	workingDirectory string
	healthHTTP       string
	healthTCP        string
	healthTimeout    time.Duration
	healthInterval   time.Duration
	restart          string
	restartDelay     time.Duration
	startAtLogin     bool
	logMode          string
	stdoutLog        string
	stderrLog        string
}

func newServiceCommand() *cobra.Command {
	var jsonOutput bool
	serviceCommand := &cobra.Command{
		Use:   "service",
		Short: "Manage durable per-user services with the native OS supervisor",
		Long: `Manage foreground service processes with launchd LaunchAgents on macOS
or systemd user units on Linux. Service definitions are versioned, contain
only non-sensitive configuration, and remain authoritative across CLI runs.`,
	}
	serviceCommand.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Emit the typed result as JSON")

	var installOptions serviceInstallOptions
	installCommand := &cobra.Command{
		Use:   "install LABEL",
		Short: "Install or atomically update a native service definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			request, err := installOptions.request(arguments[0])
			if err != nil {
				return err
			}
			plane := control.New()
			defer plane.Close()
			installed, err := plane.InstallService(command.Context(), request)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command.OutOrStdout(), installed)
			}
			action := "Installed"
			if installed.Updated {
				action = "Updated"
			}
			fmt.Fprintf(command.OutOrStdout(), "%s %s version %s\nDefinition: %s\n",
				action, installed.Ref.Label, installed.Version, installed.DefinitionPath)
			return nil
		},
	}
	installCommand.Flags().StringVar(&installOptions.version, "version", "", "Materialized service contract version")
	installCommand.Flags().StringVar(&installOptions.executable, "executable", "", "Absolute foreground executable path")
	installCommand.Flags().StringArrayVar(&installOptions.arguments, "arg", nil, "Executable argument (repeatable)")
	installCommand.Flags().StringArrayVar(&installOptions.environment, "env", nil, "Non-sensitive NAME=VALUE environment variable (repeatable)")
	installCommand.Flags().StringVar(&installOptions.workingDirectory, "working-directory", "", "Absolute service working directory")
	installCommand.Flags().StringVar(&installOptions.healthHTTP, "health-http", "", "HTTP(S) readiness URL")
	installCommand.Flags().StringVar(&installOptions.healthTCP, "health-tcp", "", "TCP readiness address as host:port")
	installCommand.Flags().DurationVar(&installOptions.healthTimeout, "health-timeout", 15*time.Second, "Readiness wait timeout")
	installCommand.Flags().DurationVar(&installOptions.healthInterval, "health-interval", 250*time.Millisecond, "Readiness retry interval")
	installCommand.Flags().StringVar(&installOptions.restart, "restart", string(control.RestartOnFailure), "Restart policy: on-failure or never")
	installCommand.Flags().DurationVar(&installOptions.restartDelay, "restart-delay", 5*time.Second, "Delay before a crash restart")
	installCommand.Flags().BoolVar(&installOptions.startAtLogin, "start-at-login", true, "Enable the service for future user logins")
	installCommand.Flags().StringVar(&installOptions.logMode, "log-mode", "", "Log routing: native or files (platform default when omitted)")
	installCommand.Flags().StringVar(&installOptions.stdoutLog, "stdout-log", "", "Absolute stdout log file for file routing")
	installCommand.Flags().StringVar(&installOptions.stderrLog, "stderr-log", "", "Absolute stderr log file for file routing")
	_ = installCommand.MarkFlagRequired("version")
	_ = installCommand.MarkFlagRequired("executable")
	serviceCommand.AddCommand(installCommand)

	serviceCommand.AddCommand(newServiceStatusCommand("start", "Start an installed service", &jsonOutput,
		func(plane control.Plane, command *cobra.Command, ref control.ServiceRef) (control.InstalledServiceStatus, error) {
			return plane.StartService(command.Context(), ref)
		}, true))
	serviceCommand.AddCommand(newServiceStatusCommand("stop", "Stop a service without triggering crash restart", &jsonOutput,
		func(plane control.Plane, command *cobra.Command, ref control.ServiceRef) (control.InstalledServiceStatus, error) {
			return plane.StopService(command.Context(), ref)
		}, false))
	serviceCommand.AddCommand(newServiceStatusCommand("restart", "Restart an installed service", &jsonOutput,
		func(plane control.Plane, command *cobra.Command, ref control.ServiceRef) (control.InstalledServiceStatus, error) {
			return plane.RestartService(command.Context(), ref)
		}, true))
	serviceCommand.AddCommand(newServiceStatusCommand("status", "Show native process and product health state", &jsonOutput,
		func(plane control.Plane, command *cobra.Command, ref control.ServiceRef) (control.InstalledServiceStatus, error) {
			return plane.ServiceStatus(command.Context(), ref)
		}, false))

	var uninstallVersion string
	uninstallCommand := &cobra.Command{
		Use:   "uninstall LABEL",
		Short: "Remove supervisor configuration while preserving product data",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			plane := control.New()
			defer plane.Close()
			request := control.UninstallServiceRequest{
				Ref:     control.ServiceRef{Label: arguments[0]},
				Version: uninstallVersion,
			}
			if err := plane.UninstallService(command.Context(), request); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(command.OutOrStdout(), struct {
					Ref         control.ServiceRef `json:"ref"`
					Uninstalled bool               `json:"uninstalled"`
				}{Ref: request.Ref, Uninstalled: true})
			}
			fmt.Fprintf(command.OutOrStdout(), "Uninstalled %s; product data and logs were preserved\n", request.Ref.Label)
			return nil
		},
	}
	uninstallCommand.Flags().StringVar(&uninstallVersion, "version", "", "Require the installed contract to have this version")
	serviceCommand.AddCommand(uninstallCommand)

	return serviceCommand
}

type serviceStatusOperation func(control.Plane, *cobra.Command, control.ServiceRef) (control.InstalledServiceStatus, error)

func newServiceStatusCommand(use, short string, jsonOutput *bool, operation serviceStatusOperation, requireHealthy bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " LABEL",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			plane := control.New()
			defer plane.Close()
			status, err := operation(plane, command, control.ServiceRef{Label: arguments[0]})
			if err != nil {
				return err
			}
			if *jsonOutput {
				if err := writeJSON(command.OutOrStdout(), status); err != nil {
					return err
				}
			} else {
				writeServiceStatus(command.OutOrStdout(), status)
			}
			if requireHealthy && status.State != control.ServiceRunningHealthy {
				return fmt.Errorf("service %s did not become healthy (state %s)", status.Ref.Label, status.State)
			}
			return nil
		},
	}
}

func (options serviceInstallOptions) request(label string) (control.InstallServiceRequest, error) {
	if options.healthHTTP != "" && options.healthTCP != "" {
		return control.InstallServiceRequest{}, fmt.Errorf("--health-http and --health-tcp are mutually exclusive")
	}
	environment := make([]control.EnvironmentVariable, 0, len(options.environment))
	for _, raw := range options.environment {
		name, value, ok := strings.Cut(raw, "=")
		if !ok || name == "" {
			return control.InstallServiceRequest{}, fmt.Errorf("environment value %q must use NAME=VALUE", raw)
		}
		environment = append(environment, control.EnvironmentVariable{Name: name, Value: value})
	}
	health := control.HealthProbe{}
	switch {
	case options.healthHTTP != "":
		health = control.HealthProbe{
			Kind:     control.HealthProbeHTTP,
			Target:   options.healthHTTP,
			Timeout:  options.healthTimeout,
			Interval: options.healthInterval,
		}
	case options.healthTCP != "":
		health = control.HealthProbe{
			Kind:     control.HealthProbeTCP,
			Target:   options.healthTCP,
			Timeout:  options.healthTimeout,
			Interval: options.healthInterval,
		}
	}
	logMode := control.LogMode(options.logMode)
	if logMode == "" && (options.stdoutLog != "" || options.stderrLog != "") {
		logMode = control.LogFiles
	}
	return control.InstallServiceRequest{
		Ref:              control.ServiceRef{Label: label},
		Version:          options.version,
		Executable:       options.executable,
		Arguments:        append([]string(nil), options.arguments...),
		Environment:      environment,
		WorkingDirectory: options.workingDirectory,
		Health:           health,
		Restart:          control.RestartPolicy(options.restart),
		RestartDelay:     options.restartDelay,
		StartAtLogin:     options.startAtLogin,
		Logs: control.LogRouting{
			Mode:       logMode,
			StdoutPath: options.stdoutLog,
			StderrPath: options.stderrLog,
		},
	}, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeServiceStatus(output io.Writer, status control.InstalledServiceStatus) {
	fmt.Fprintf(output, "%s: %s\n", status.Ref.Label, status.State)
	if status.Version != "" {
		fmt.Fprintf(output, "  Version: %s\n", status.Version)
	}
	fmt.Fprintf(output, "  Manager: %s\n", status.Diagnostics.Manager)
	if status.Diagnostics.PID > 0 {
		fmt.Fprintf(output, "  PID: %d\n", status.Diagnostics.PID)
	}
	if status.Diagnostics.NativeState != "" {
		fmt.Fprintf(output, "  Native state: %s\n", status.Diagnostics.NativeState)
	}
	if status.Diagnostics.ExitCode != nil {
		fmt.Fprintf(output, "  Exit code: %d\n", *status.Diagnostics.ExitCode)
	}
	if status.Diagnostics.ExitReason != "" {
		fmt.Fprintf(output, "  Exit reason: %s\n", status.Diagnostics.ExitReason)
	}
	if status.Diagnostics.Message != "" {
		fmt.Fprintf(output, "  Detail: %s\n", status.Diagnostics.Message)
	}
	if len(status.Diagnostics.LogPaths) > 0 {
		fmt.Fprintf(output, "  Logs: %s\n", strings.Join(status.Diagnostics.LogPaths, ", "))
	}
	if len(status.Diagnostics.RecentLogs) > 0 {
		fmt.Fprintln(output, "  Recent logs:")
		for _, line := range status.Diagnostics.RecentLogs {
			fmt.Fprintf(output, "    %s\n", line)
		}
	}
}
