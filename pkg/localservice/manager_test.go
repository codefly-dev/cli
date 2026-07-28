package localservice

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSystemdLifecycleUsesUserManagerAndPreservesProductData(t *testing.T) {
	home := t.TempDir()
	tracePath := filepath.Join(t.TempDir(), "trace")
	statePath := filepath.Join(t.TempDir(), "state")
	installNativeTool(t, "systemctl", systemctlFixture)
	installNativeTool(t, "journalctl", "#!/bin/sh\nprintf 'recent journal line\\n'\n")
	t.Setenv("CODEFLY_TEST_TRACE", tracePath)
	t.Setenv("CODEFLY_TEST_STATE", statePath)
	t.Setenv("XDG_CONFIG_HOME", "")

	manager := newManager("linux", home, os.Getuid(), executeCommand)
	request := testRequest(t)
	request.StartAtLogin = true
	request.Logs = LogRouting{Mode: LogNative}
	installed, err := manager.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Updated {
		t.Fatal("fresh installation reported an update")
	}
	assertMode(t, installed.DefinitionPath, 0o600)

	status, err := manager.StartService(context.Background(), request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceRunningHealthy || status.Diagnostics.PID != 4321 {
		t.Fatalf("started status = %#v", status)
	}
	status, err = manager.StopService(context.Background(), request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceInstalledStopped {
		t.Fatalf("stopped status = %#v", status)
	}
	status, err = manager.RestartService(context.Background(), request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceRunningHealthy {
		t.Fatalf("restarted status = %#v", status)
	}

	productData := filepath.Join(home, ".codefly", "product.db")
	if err := os.MkdirAll(filepath.Dir(productData), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(productData, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.UninstallService(context.Background(), UninstallServiceRequest{
		Ref: request.Ref, Version: request.Version,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installed.DefinitionPath); !os.IsNotExist(err) {
		t.Fatalf("definition still exists: %v", err)
	}
	if data, err := os.ReadFile(productData); err != nil || string(data) != "preserve" {
		t.Fatalf("product data changed: %q, %v", data, err)
	}

	trace := readFile(t, tracePath)
	for _, expected := range []string{
		"--user daemon-reload",
		"--user enable dev.codefly.test.service",
		"--user start dev.codefly.test.service",
		"--user stop dev.codefly.test.service",
		"--user restart dev.codefly.test.service",
		"--user disable dev.codefly.test.service",
		"--user reset-failed",
	} {
		if !strings.Contains(trace, expected) {
			t.Errorf("systemctl trace does not contain %q:\n%s", expected, trace)
		}
	}
}

func TestLaunchdLifecycleUsesModernPerUserCommands(t *testing.T) {
	home := t.TempDir()
	tracePath := filepath.Join(t.TempDir(), "trace")
	statePath := filepath.Join(t.TempDir(), "state")
	installNativeTool(t, "launchctl", launchctlFixture)
	t.Setenv("CODEFLY_TEST_TRACE", tracePath)
	t.Setenv("CODEFLY_TEST_STATE", statePath)

	manager := newManager("darwin", home, 501, executeCommand)
	request := testRequest(t)
	request.StartAtLogin = true
	request.Logs = LogRouting{
		Mode:       LogFiles,
		StdoutPath: filepath.Join(home, "logs", "stdout.log"),
		StderrPath: filepath.Join(home, "logs", "stderr.log"),
	}
	if _, err := manager.InstallService(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	status, err := manager.StartService(context.Background(), request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceRunningHealthy || status.Diagnostics.PID != 9876 {
		t.Fatalf("started status = %#v", status)
	}
	if _, err := manager.StopService(context.Background(), request.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RestartService(context.Background(), request.Ref); err != nil {
		t.Fatal(err)
	}
	if err := manager.UninstallService(context.Background(), UninstallServiceRequest{Ref: request.Ref}); err != nil {
		t.Fatal(err)
	}

	trace := readFile(t, tracePath)
	for _, expected := range []string{
		"bootstrap gui/501 ",
		"kickstart -k gui/501/dev.codefly.test",
		"bootout gui/501/dev.codefly.test",
		"print gui/501/dev.codefly.test",
	} {
		if !strings.Contains(trace, expected) {
			t.Errorf("launchctl trace does not contain %q:\n%s", expected, trace)
		}
	}
	if strings.Contains(trace, " load ") || strings.Contains(trace, " unload ") {
		t.Fatalf("legacy launchctl command used:\n%s", trace)
	}
}

func TestInstallRequiresVersionChangeForMaterializedChanges(t *testing.T) {
	home := t.TempDir()
	installNativeTool(t, "systemctl", systemctlFixture)
	t.Setenv("CODEFLY_TEST_TRACE", filepath.Join(t.TempDir(), "trace"))
	t.Setenv("CODEFLY_TEST_STATE", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_CONFIG_HOME", "")

	manager := newManager("linux", home, os.Getuid(), executeCommand)
	request := testRequest(t)
	request.Logs = LogRouting{Mode: LogNative}
	first, err := manager.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := manager.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Updated {
		t.Fatal("idempotent installation reported an update")
	}
	changedExecutable := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(changedExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	request.Executable = changedExecutable
	if _, err := manager.InstallService(context.Background(), request); err == nil {
		t.Fatal("same-version executable rebind was accepted")
	}
	request.Version = "2"
	second, err := manager.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Updated || second.DefinitionPath != first.DefinitionPath {
		t.Fatalf("updated installation = %#v", second)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(first.DefinitionPath), request.Ref.Label+".service*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != first.DefinitionPath {
		t.Fatalf("service definitions after update = %v", matches)
	}
}

func TestInstallRollsBackDefinitionWhenSupervisorRejectsIt(t *testing.T) {
	home := t.TempDir()
	installNativeTool(t, "systemctl", "#!/bin/sh\nif [ \"$2\" = \"enable\" ]; then printf 'enable failed\\n' >&2; exit 1; fi\n")
	t.Setenv("XDG_CONFIG_HOME", "")
	manager := newManager("linux", home, os.Getuid(), executeCommand)
	request := testRequest(t)
	request.StartAtLogin = true
	request.Logs = LogRouting{Mode: LogNative}

	if _, err := manager.InstallService(context.Background(), request); err == nil {
		t.Fatal("installation succeeded despite supervisor rejection")
	}
	path, err := manager.definitionPath(request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed installation left a definition: %v", err)
	}
}

func TestServiceStatusDetectsUnsafeAndCorruptDefinitions(t *testing.T) {
	home := t.TempDir()
	installNativeTool(t, "systemctl", systemctlFixture)
	t.Setenv("CODEFLY_TEST_TRACE", filepath.Join(t.TempDir(), "trace"))
	t.Setenv("CODEFLY_TEST_STATE", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_CONFIG_HOME", "")
	manager := newManager("linux", home, os.Getuid(), executeCommand)
	request := testRequest(t)
	request.Logs = LogRouting{Mode: LogNative}
	installed, err := manager.InstallService(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(installed.DefinitionPath, 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := manager.ServiceStatus(context.Background(), request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStaleCorrupt {
		t.Fatalf("unsafe definition status = %#v", status)
	}
	if err := os.Chmod(installed.DefinitionPath, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(installed.DefinitionPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("# tampered\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	status, err = manager.ServiceStatus(context.Background(), request.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStaleCorrupt {
		t.Fatalf("corrupt definition status = %#v", status)
	}
	if err := manager.UninstallService(context.Background(), UninstallServiceRequest{Ref: request.Ref}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installed.DefinitionPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt definition still exists after uninstall: %v", err)
	}
}

func TestHealthChecksUseRealHTTPAndTCPBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	healthy, err := checkHealth(context.Background(), HealthProbe{Kind: HealthProbeHTTP, Target: server.URL}, time.Second)
	if err != nil || !healthy {
		t.Fatalf("HTTP health = %t, %v", healthy, err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()
	healthy, err = checkHealth(context.Background(), HealthProbe{
		Kind: HealthProbeTCP, Target: listener.Addr().String(),
	}, time.Second)
	if err != nil || !healthy {
		t.Fatalf("TCP health = %t, %v", healthy, err)
	}
	<-accepted
}

func TestNativeStatusMappings(t *testing.T) {
	manager := newManager("linux", t.TempDir(), os.Getuid(), nil)
	base := ServiceStatus{Ref: ServiceRef{Label: "dev.codefly.test"}, Installed: true}
	manager.run = func(_ context.Context, _ string, _ ...string) (string, error) {
		return strings.Join([]string{
			"LoadState=loaded",
			"ActiveState=failed",
			"SubState=failed",
			"MainPID=0",
			"ExecMainStatus=1",
			"ExecMainCode=exited",
			"NRestarts=5",
			"Result=start-limit-hit",
		}, "\n"), nil
	}
	status, err := manager.systemdStatus(context.Background(), InstallServiceRequest{
		Ref: ServiceRef{Label: "dev.codefly.test"},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceCrashLooping || status.Diagnostics.RestartCount != 5 {
		t.Fatalf("systemd crash status = %#v", status)
	}

	manager.platform = "darwin"
	manager.run = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "state = waiting\nruns = 6\nlast exit code = 78\n", nil
	}
	status, err = manager.launchdStatus(context.Background(), InstallServiceRequest{
		Ref: ServiceRef{Label: "dev.codefly.test"},
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceCrashLooping || status.Diagnostics.ExitCode == nil || *status.Diagnostics.ExitCode != 78 {
		t.Fatalf("launchd crash status = %#v", status)
	}
}

func installNativeTool(t *testing.T, name, content string) {
	t.Helper()
	binDirectory := os.Getenv("CODEFLY_TEST_BIN")
	if binDirectory == "" {
		binDirectory = t.TempDir()
		t.Setenv("CODEFLY_TEST_BIN", binDirectory)
		t.Setenv("PATH", binDirectory)
	}
	path := filepath.Join(binDirectory, name)
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

const systemctlFixture = `#!/bin/sh
printf '%s\n' "$*" >> "$CODEFLY_TEST_TRACE"
case "$2" in
  start|restart|try-restart)
    printf 'active\n' > "$CODEFLY_TEST_STATE"
    ;;
  stop)
    printf 'inactive\n' > "$CODEFLY_TEST_STATE"
    ;;
  show)
    if [ -f "$CODEFLY_TEST_STATE" ]; then
      IFS= read -r service_state < "$CODEFLY_TEST_STATE"
    else
      service_state=inactive
    fi
    if [ "$service_state" = active ]; then
      printf 'LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=4321\nExecMainStatus=0\nExecMainCode=0\nNRestarts=0\nResult=success\n'
    else
      printf 'LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nExecMainStatus=0\nExecMainCode=0\nNRestarts=0\nResult=success\n'
    fi
    ;;
esac
`

const launchctlFixture = `#!/bin/sh
printf '%s\n' "$*" >> "$CODEFLY_TEST_TRACE"
case "$1" in
  bootstrap|kickstart)
    printf 'loaded\n' > "$CODEFLY_TEST_STATE"
    ;;
  bootout)
    if [ ! -f "$CODEFLY_TEST_STATE" ]; then
      printf 'Could not find service\n' >&2
      exit 1
    fi
    /bin/rm "$CODEFLY_TEST_STATE"
    ;;
  print)
    if [ ! -f "$CODEFLY_TEST_STATE" ]; then
      printf 'Could not find service\n' >&2
      exit 1
    fi
    printf 'state = running\npid = 9876\nruns = 1\nlast exit code = 0\n'
    ;;
esac
`

func TestParseExitCode(t *testing.T) {
	value := parseExitCode(strconv.Itoa(17))
	if value == nil || *value != 17 {
		t.Fatalf("exit code = %v", value)
	}
}
