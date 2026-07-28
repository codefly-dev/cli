package cmd

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/control"
	"github.com/spf13/cobra"
)

func TestServiceInstallOptionsBuildTypedContract(t *testing.T) {
	options := serviceInstallOptions{
		version:           "2026.07.28",
		executable:        "/opt/codefly/mind-server",
		publicArguments:   []string{"serve", "--foreground"},
		publicEnvironment: []string{"MODE=embedded", "VALUE=contains=equals"},
		workingDirectory:  "/opt/codefly",
		healthHTTP:        "http://127.0.0.1:17400/healthz",
		healthTimeout:     20 * time.Second,
		healthInterval:    time.Second,
		restart:           "on-failure",
		restartDelay:      3 * time.Second,
		startAtLogin:      true,
		logMode:           "files",
		stdoutLog:         "/tmp/mind.stdout.log",
		stderrLog:         "/tmp/mind.stderr.log",
	}
	request, err := options.request("dev.codefly.mind")
	if err != nil {
		t.Fatal(err)
	}
	if request.Ref.Label != "dev.codefly.mind" || request.Version != options.version {
		t.Fatalf("identity = %#v, version = %q", request.Ref, request.Version)
	}
	wantArguments := []control.ServiceArgument{
		{Value: "serve", Classification: control.ValuePublic},
		{Value: "--foreground", Classification: control.ValuePublic},
	}
	if !reflect.DeepEqual(request.Arguments, wantArguments) {
		t.Fatalf("arguments = %v", request.Arguments)
	}
	wantEnvironment := []control.EnvironmentVariable{
		{Name: "MODE", Value: "embedded", Classification: control.ValuePublic},
		{Name: "VALUE", Value: "contains=equals", Classification: control.ValuePublic},
	}
	if !reflect.DeepEqual(request.Environment, wantEnvironment) {
		t.Fatalf("environment = %#v", request.Environment)
	}
	if request.Health.Kind != control.HealthProbeHTTP || request.Health.Target != options.healthHTTP {
		t.Fatalf("health = %#v", request.Health)
	}
	if request.Restart != control.RestartOnFailure || request.Logs.Mode != control.LogFiles {
		t.Fatalf("restart = %q, logs = %#v", request.Restart, request.Logs)
	}
}

func TestServiceInstallOptionsRejectAmbiguousHealthAndEnvironment(t *testing.T) {
	if _, err := (serviceInstallOptions{
		healthHTTP: "http://127.0.0.1/healthz",
		healthTCP:  "127.0.0.1:8080",
	}).request("dev.codefly.test"); err == nil {
		t.Fatal("two health probes were accepted")
	}
	if _, err := (serviceInstallOptions{
		publicEnvironment: []string{"MISSING_VALUE"},
	}).request("dev.codefly.test"); err == nil {
		t.Fatal("malformed environment was accepted")
	}
}

func TestServiceCommandExposesCompleteLifecycle(t *testing.T) {
	command := newServiceCommand()
	for _, name := range []string{"install", "start", "stop", "restart", "status", "uninstall"} {
		found, _, err := command.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if found == command || found.Name() != name {
			t.Errorf("service subcommand %q is missing", name)
		}
	}
	install, _, err := command.Find([]string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"public-arg", "public-env"} {
		if install.Flags().Lookup(name) == nil {
			t.Errorf("install flag --%s is missing", name)
		}
	}
	for _, name := range []string{"arg", "env"} {
		if install.Flags().Lookup(name) != nil {
			t.Errorf("unsafe literal flag --%s is still exposed", name)
		}
	}
}

func TestWriteServiceStatusIncludesTypedDiagnostics(t *testing.T) {
	exitCode := 78
	var output bytes.Buffer
	if err := writeServiceStatus(&output, control.InstalledServiceStatus{
		Ref:             control.ServiceRef{Label: "dev.codefly.mind"},
		Version:         "2",
		State:           control.ServiceCrashLooping,
		OperatorStopped: true,
		Diagnostics: control.ServiceDiagnostic{
			Manager:      "launchd",
			NativeState:  "waiting",
			ExitCode:     &exitCode,
			ExitReason:   "configuration",
			RestartCount: 5,
			RecentLogs:   []string{"mind failed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"dev.codefly.mind: crash-looping",
		"Version: 2",
		"Manager: launchd",
		"Native state: waiting",
		"Operator stopped: true",
		"Exit code: 78",
		"Exit reason: configuration",
		"mind failed",
	} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Errorf("status output does not contain %q:\n%s", expected, output.String())
		}
	}
}

func TestJSONServiceFailureIsMarkedMachineReadable(t *testing.T) {
	jsonOutput := true
	command := newServiceStatusCommand("start", "start", &jsonOutput,
		func(control.Plane, *cobra.Command, control.ServiceRef) (control.InstalledServiceStatus, error) {
			return control.InstalledServiceStatus{
				Ref:   control.ServiceRef{Label: "dev.codefly.test"},
				State: control.ServiceRunningUnhealthy,
			}, nil
		}, true)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"dev.codefly.test"})
	err := command.Execute()
	if err == nil {
		t.Fatal("unhealthy start succeeded")
	}
	if !IsMachineReadableError(err) {
		t.Fatalf("JSON service failure was not marked machine-readable: %v", err)
	}
	var status control.InstalledServiceStatus
	if decodeErr := json.Unmarshal(output.Bytes(), &status); decodeErr != nil {
		t.Fatalf("service failure output is not JSON: %v\n%s", decodeErr, output.String())
	}
	if status.State != control.ServiceRunningUnhealthy {
		t.Fatalf("service failure status = %#v", status)
	}
}
