package localservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const contractMarker = "codefly-service-contract:"

func renderDefinition(platform string, request InstallServiceRequest) ([]byte, error) {
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	contract, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal service contract: %w", err)
	}
	encoded := base64.RawStdEncoding.EncodeToString(contract)
	switch platform {
	case "darwin":
		return renderLaunchAgent(request, encoded)
	case "linux":
		return renderSystemdUnit(request, encoded), nil
	default:
		return nil, unsupportedPlatform(platform)
	}
}

func renderLaunchAgent(request InstallServiceRequest, encodedContract string) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	out.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	fmt.Fprintf(&out, "<!-- %s%s -->\n", contractMarker, encodedContract)
	out.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	writePlistString(&out, "Label", request.Ref.Label)
	out.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	if err := writePlistValue(&out, "    ", request.Executable); err != nil {
		return nil, err
	}
	for _, argument := range request.Arguments {
		if err := writePlistValue(&out, "    ", argument.Value); err != nil {
			return nil, err
		}
	}
	out.WriteString("  </array>\n")
	if request.WorkingDirectory != "" {
		writePlistString(&out, "WorkingDirectory", request.WorkingDirectory)
	}
	if len(request.Environment) > 0 {
		out.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		environment := append([]EnvironmentVariable(nil), request.Environment...)
		sort.Slice(environment, func(i, j int) bool { return environment[i].Name < environment[j].Name })
		for _, variable := range environment {
			if err := writePlistKeyValue(&out, "    ", variable.Name, variable.Value); err != nil {
				return nil, err
			}
		}
		out.WriteString("  </dict>\n")
	}
	fmt.Fprintf(&out, "  <key>RunAtLoad</key>\n  <%t/>\n", request.StartAtLogin)
	switch request.Restart {
	case RestartOnFailure:
		out.WriteString("  <key>KeepAlive</key>\n  <dict>\n")
		out.WriteString("    <key>SuccessfulExit</key>\n    <false/>\n")
		out.WriteString("  </dict>\n")
		fmt.Fprintf(&out, "  <key>ThrottleInterval</key>\n  <integer>%d</integer>\n", restartDelaySeconds(request))
	case RestartNever:
		out.WriteString("  <key>KeepAlive</key>\n  <false/>\n")
	}
	if request.Logs.Mode == LogFiles {
		writePlistString(&out, "StandardOutPath", request.Logs.StdoutPath)
		writePlistString(&out, "StandardErrorPath", request.Logs.StderrPath)
	}
	out.WriteString("  <key>Umask</key>\n  <integer>63</integer>\n")
	out.WriteString("</dict>\n</plist>\n")
	return out.Bytes(), nil
}

func writePlistString(out *bytes.Buffer, key, value string) {
	_ = writePlistKeyValue(out, "  ", key, value)
}

func writePlistKeyValue(out *bytes.Buffer, indent, key, value string) error {
	out.WriteString(indent)
	out.WriteString("<key>")
	if err := xml.EscapeText(out, []byte(key)); err != nil {
		return fmt.Errorf("escape plist key: %w", err)
	}
	out.WriteString("</key>\n")
	return writePlistValue(out, indent, value)
}

func writePlistValue(out *bytes.Buffer, indent, value string) error {
	out.WriteString(indent)
	out.WriteString("<string>")
	if err := xml.EscapeText(out, []byte(value)); err != nil {
		return fmt.Errorf("escape plist value: %w", err)
	}
	out.WriteString("</string>\n")
	return nil
}

func renderSystemdUnit(request InstallServiceRequest, encodedContract string) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s%s\n", contractMarker, encodedContract)
	out.WriteString("[Unit]\n")
	fmt.Fprintf(&out, "Description=Codefly local service %s\n", request.Ref.Label)
	out.WriteString("StartLimitIntervalSec=60\n")
	out.WriteString("StartLimitBurst=5\n\n")
	out.WriteString("[Service]\n")
	out.WriteString("Type=simple\n")
	out.WriteString("ExecStart=")
	out.WriteString(systemdExecQuote(request.Executable))
	for _, argument := range request.Arguments {
		out.WriteByte(' ')
		out.WriteString(systemdExecQuote(argument.Value))
	}
	out.WriteByte('\n')
	if request.WorkingDirectory != "" {
		fmt.Fprintf(&out, "WorkingDirectory=%s\n", systemdQuote(request.WorkingDirectory))
	}
	environment := append([]EnvironmentVariable(nil), request.Environment...)
	sort.Slice(environment, func(i, j int) bool { return environment[i].Name < environment[j].Name })
	for _, variable := range environment {
		fmt.Fprintf(&out, "Environment=%s\n", systemdQuote(variable.Name+"="+variable.Value))
	}
	restart := "no"
	if request.Restart == RestartOnFailure {
		restart = "on-failure"
	}
	fmt.Fprintf(&out, "Restart=%s\n", restart)
	if request.Restart == RestartOnFailure {
		fmt.Fprintf(&out, "RestartSec=%ds\n", restartDelaySeconds(request))
	}
	out.WriteString("KillSignal=SIGTERM\n")
	out.WriteString("TimeoutStopSec=10s\n")
	out.WriteString("UMask=0077\n")
	if request.Logs.Mode == LogFiles {
		fmt.Fprintf(&out, "StandardOutput=%s\n", systemdQuote("append:"+request.Logs.StdoutPath))
		fmt.Fprintf(&out, "StandardError=%s\n", systemdQuote("append:"+request.Logs.StderrPath))
	} else {
		out.WriteString("StandardOutput=journal\n")
		out.WriteString("StandardError=journal\n")
	}
	out.WriteString("\n[Install]\nWantedBy=default.target\n")
	return []byte(out.String())
}

func systemdQuote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '%':
			out.WriteString("%%")
		case '\\', '"':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

func systemdExecQuote(value string) string {
	return strings.ReplaceAll(systemdQuote(value), "$", "$$")
}

func restartDelaySeconds(request InstallServiceRequest) int64 {
	seconds := int64(request.RestartDelay / time.Second)
	if seconds < 1 {
		return 5
	}
	return seconds
}

func contractFromDefinition(definition []byte) (InstallServiceRequest, error) {
	for _, line := range strings.Split(string(definition), "\n") {
		index := strings.Index(line, contractMarker)
		if index < 0 {
			continue
		}
		encoded := strings.TrimSpace(strings.TrimSuffix(line[index+len(contractMarker):], "-->"))
		raw, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return InstallServiceRequest{}, fmt.Errorf("decode embedded service contract: %w", err)
		}
		var request InstallServiceRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return InstallServiceRequest{}, fmt.Errorf("parse embedded service contract: %w", err)
		}
		return request, nil
	}
	return InstallServiceRequest{}, fmt.Errorf("service definition has no Codefly contract metadata")
}

func validateDefinition(platform string, definition []byte) (InstallServiceRequest, error) {
	request, err := contractFromDefinition(definition)
	if err != nil {
		return InstallServiceRequest{}, err
	}
	expected, err := renderDefinition(platform, request)
	if err != nil {
		return InstallServiceRequest{}, err
	}
	if !bytes.Equal(definition, expected) {
		return InstallServiceRequest{}, fmt.Errorf("service definition does not match its materialized contract")
	}
	return request, nil
}

func definitionName(platform, label string) string {
	if platform == "darwin" {
		return label + ".plist"
	}
	return label + ".service"
}

func parseExitCode(value string) *int {
	code, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	return &code
}

func cleanAbsolute(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}
