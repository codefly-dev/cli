package localservice

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNativeUserManagerLifecycle(t *testing.T) {
	if os.Getenv("CODEFLY_SERVICE_INTEGRATION") != "1" {
		t.Skip("set CODEFLY_SERVICE_INTEGRATION=1 to exercise the real user service manager")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native user services are unsupported on this platform")
	}

	installation, err := New()
	if err != nil {
		t.Fatal(err)
	}
	label := "dev.codefly.integration.lifecycle"
	ref := ServiceRef{Label: label}
	t.Cleanup(func() {
		_ = installation.UninstallService(context.Background(), UninstallServiceRequest{Ref: ref})
	})

	logDirectory := t.TempDir()
	request := InstallServiceRequest{
		Ref:          ref,
		Version:      "1",
		Executable:   "/bin/sh",
		Arguments:    []string{"-c", "while :; do /bin/sleep 1; done"},
		Restart:      RestartOnFailure,
		RestartDelay: time.Second,
		StartAtLogin: false,
		Logs: LogRouting{
			Mode:       LogFiles,
			StdoutPath: filepath.Join(logDirectory, "stdout.log"),
			StderrPath: filepath.Join(logDirectory, "stderr.log"),
		},
	}
	installed, err := installation.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	status, err := installation.StartService(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceRunningHealthy || status.Diagnostics.PID == 0 {
		t.Fatalf("started status = %#v", status)
	}

	firstPID := status.Diagnostics.PID
	process, err := os.FindProcess(firstPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	status = waitForNewPID(t, installation, ref, firstPID)
	if status.State != ServiceRunningHealthy {
		t.Fatalf("post-crash status = %#v", status)
	}

	status, err = installation.StopService(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceInstalledStopped {
		t.Fatalf("explicit stop status = %#v", status)
	}
	time.Sleep(2 * time.Second)
	status, err = installation.ServiceStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceInstalledStopped {
		t.Fatalf("service restarted after explicit stop: %#v", status)
	}

	status, err = installation.RestartService(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceRunningHealthy {
		t.Fatalf("explicit restart status = %#v", status)
	}

	request.Version = "2"
	request.Environment = []EnvironmentVariable{{Name: "CODEFLY_INTEGRATION_VERSION", Value: "2"}}
	updated, err := installation.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Updated || updated.DefinitionPath != installed.DefinitionPath {
		t.Fatalf("updated installation = %#v", updated)
	}
	status, err = installation.ServiceStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "2" || status.State != ServiceRunningHealthy {
		t.Fatalf("updated status = %#v", status)
	}

	if err := installation.UninstallService(context.Background(), UninstallServiceRequest{
		Ref: ref, Version: "2",
	}); err != nil {
		t.Fatal(err)
	}
	status, err = installation.ServiceStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceNotInstalled {
		t.Fatalf("uninstalled status = %#v", status)
	}
}

func waitForNewPID(t *testing.T, installation Installation, ref ServiceRef, previous int) ServiceStatus {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := installation.ServiceStatus(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.Diagnostics.PID != 0 && status.Diagnostics.PID != previous {
			return status
		}
		select {
		case <-deadline.C:
			t.Fatalf("service did not restart after PID %d was killed; last status: %#v", previous, status)
		case <-ticker.C:
		}
	}
}
