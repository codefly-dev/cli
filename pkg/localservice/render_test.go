package localservice

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRenderLaunchAgentRoundTripsMaterializedContract(t *testing.T) {
	request := testRequest(t)
	request.Arguments = []string{"serve", `value with "quotes" & <xml>`}
	request.Environment = []EnvironmentVariable{
		{Name: "Z_LAST", Value: "z"},
		{Name: "A_FIRST", Value: `a&<"value">`},
	}
	request.StartAtLogin = true
	request.Logs = LogRouting{
		Mode:       LogFiles,
		StdoutPath: filepath.Join(t.TempDir(), "stdout.log"),
		StderrPath: filepath.Join(t.TempDir(), "stderr.log"),
	}

	definition, err := renderDefinition("darwin", request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(definition)
	for _, expected := range []string{
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>SuccessfulExit</key>\n    <false/>",
		"<key>ThrottleInterval</key>\n  <integer>5</integer>",
		"a&amp;&lt;&#34;value&#34;&gt;",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("LaunchAgent does not contain %q:\n%s", expected, text)
		}
	}
	if strings.Index(text, "A_FIRST") > strings.Index(text, "Z_LAST") {
		t.Fatal("environment variables were not rendered deterministically")
	}
	roundTripped, err := validateDefinition("darwin", definition)
	if err != nil {
		t.Fatal(err)
	}
	if roundTripped.Version != request.Version || roundTripped.Executable != request.Executable {
		t.Fatalf("round-tripped contract = %#v", roundTripped)
	}

	if runtime.GOOS == "darwin" {
		path := filepath.Join(t.TempDir(), "service.plist")
		if err := os.WriteFile(path, definition, 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil rejected LaunchAgent: %v\n%s", err, output)
		}
	}
}

func TestRenderSystemdUnitUsesForegroundAndCrashOnlyRestart(t *testing.T) {
	request := testRequest(t)
	request.Arguments = []string{"serve", `value with "quotes"`, `$RUNTIME %n`}
	request.Environment = []EnvironmentVariable{{Name: "PUBLIC_SETTING", Value: `a\b"c`}}
	request.Logs = LogRouting{Mode: LogNative}

	definition, err := renderDefinition("linux", request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(definition)
	for _, expected := range []string{
		"Type=simple",
		`ExecStart="` + request.Executable + `" "serve" "value with \"quotes\""`,
		`"$$RUNTIME %%n"`,
		`Environment="PUBLIC_SETTING=a\\b\"c"`,
		"Restart=on-failure",
		"StartLimitBurst=5",
		"StandardOutput=journal",
		"WantedBy=default.target",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("systemd unit does not contain %q:\n%s", expected, text)
		}
	}
	if _, err := validateDefinition("linux", definition); err != nil {
		t.Fatal(err)
	}
}

func TestRenderSystemdNeverPolicyAndFileLogs(t *testing.T) {
	request := testRequest(t)
	request.Restart = RestartNever
	logDirectory := filepath.Join(t.TempDir(), "logs with % specifier")
	request.Logs = LogRouting{
		Mode:       LogFiles,
		StdoutPath: filepath.Join(logDirectory, "stdout.log"),
		StderrPath: filepath.Join(logDirectory, "stderr.log"),
	}
	definition, err := renderDefinition("linux", request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(definition)
	for _, expected := range []string{
		"Restart=no",
		`StandardOutput="append:` + strings.ReplaceAll(request.Logs.StdoutPath, "%", "%%") + `"`,
		`StandardError="append:` + strings.ReplaceAll(request.Logs.StderrPath, "%", "%%") + `"`,
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("systemd unit does not contain %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "Restart=never") {
		t.Fatalf("neutral restart policy leaked into systemd unit:\n%s", text)
	}
}

func TestRenderRejectsSensitiveEnvironment(t *testing.T) {
	request := testRequest(t)
	request.Logs = LogRouting{Mode: LogNative}
	request.Environment = []EnvironmentVariable{{
		Name:      "PROVIDER_TOKEN",
		Value:     "must-not-appear",
		Sensitive: true,
	}}
	definition, err := renderDefinition("linux", request)
	if err == nil {
		t.Fatalf("sensitive environment was rendered:\n%s", definition)
	}
	if strings.Contains(string(definition), "must-not-appear") {
		t.Fatal("sensitive environment value appeared in a definition")
	}
}

func TestDefinitionValidationDetectsTampering(t *testing.T) {
	request := testRequest(t)
	request.Logs = LogRouting{Mode: LogNative}
	definition, err := renderDefinition("linux", request)
	if err != nil {
		t.Fatal(err)
	}
	definition = append(definition, []byte("# changed outside Codefly\n")...)
	if _, err := validateDefinition("linux", definition); err == nil {
		t.Fatal("tampered definition was accepted")
	}
}

func TestRenderRejectsUnsupportedControlCharacters(t *testing.T) {
	request := testRequest(t)
	request.Logs = LogRouting{Mode: LogNative}
	request.Arguments = []string{"invalid\x00argument"}
	if _, err := renderDefinition("linux", request); err == nil {
		t.Fatal("argument containing NUL was accepted")
	}
}

func testRequest(t *testing.T) InstallServiceRequest {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "service")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return InstallServiceRequest{
		Ref:          ServiceRef{Label: "dev.codefly.test"},
		Version:      "1",
		Executable:   executable,
		Restart:      RestartOnFailure,
		RestartDelay: 5 * time.Second,
	}
}
