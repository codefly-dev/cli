package cmd

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/control"
)

func TestServiceInstallOptionsBuildTypedContract(t *testing.T) {
	options := serviceInstallOptions{
		version:          "2026.07.28",
		executable:       "/opt/codefly/mind-server",
		arguments:        []string{"serve", "--foreground"},
		environment:      []string{"MODE=embedded", "VALUE=contains=equals"},
		workingDirectory: "/opt/codefly",
		healthHTTP:       "http://127.0.0.1:17400/healthz",
		healthTimeout:    20 * time.Second,
		healthInterval:   time.Second,
		restart:          "on-failure",
		restartDelay:     3 * time.Second,
		startAtLogin:     true,
		logMode:          "files",
		stdoutLog:        "/tmp/mind.stdout.log",
		stderrLog:        "/tmp/mind.stderr.log",
	}
	request, err := options.request("dev.codefly.mind")
	if err != nil {
		t.Fatal(err)
	}
	if request.Ref.Label != "dev.codefly.mind" || request.Version != options.version {
		t.Fatalf("identity = %#v, version = %q", request.Ref, request.Version)
	}
	if !reflect.DeepEqual(request.Arguments, options.arguments) {
		t.Fatalf("arguments = %v", request.Arguments)
	}
	wantEnvironment := []control.EnvironmentVariable{
		{Name: "MODE", Value: "embedded"},
		{Name: "VALUE", Value: "contains=equals"},
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
		environment: []string{"MISSING_VALUE"},
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
}

func TestWriteServiceStatusIncludesTypedDiagnostics(t *testing.T) {
	exitCode := 78
	var output bytes.Buffer
	writeServiceStatus(&output, control.InstalledServiceStatus{
		Ref:     control.ServiceRef{Label: "dev.codefly.mind"},
		Version: "2",
		State:   control.ServiceCrashLooping,
		Diagnostics: control.ServiceDiagnostic{
			Manager:      "launchd",
			NativeState:  "waiting",
			ExitCode:     &exitCode,
			ExitReason:   "configuration",
			RestartCount: 5,
			RecentLogs:   []string{"mind failed"},
		},
	})
	for _, expected := range []string{
		"dev.codefly.mind: crash-looping",
		"Version: 2",
		"Manager: launchd",
		"Native state: waiting",
		"Exit code: 78",
		"Exit reason: configuration",
		"mind failed",
	} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Errorf("status output does not contain %q:\n%s", expected, output.String())
		}
	}
}
