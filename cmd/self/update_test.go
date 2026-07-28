package self

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cliupdate"
	"github.com/codefly-dev/core/releaseupdate"
)

type fakeUpdateService struct {
	installation cliupdate.Installation
	result       cliupdate.CheckResult
	checkErr     error
	applyResult  releaseupdate.ApplyResult
	applyErr     error
	checked      bool
	applied      bool
	downgrade    bool
}

func (service *fakeUpdateService) Installation() cliupdate.Installation {
	return service.installation
}

func (service *fakeUpdateService) Check(
	_ context.Context,
	_ releaseupdate.Channel,
	allowDowngrade bool,
) (cliupdate.CheckResult, error) {
	service.checked = true
	service.downgrade = allowDowngrade
	return service.result, service.checkErr
}

func (service *fakeUpdateService) StageAndApply(
	_ context.Context,
	_ *cliupdate.CheckResult,
) (releaseupdate.ApplyResult, error) {
	service.applied = true
	return service.applyResult, service.applyErr
}

func TestRunUpdateRefusesExternallyManagedInstallations(t *testing.T) {
	service := &fakeUpdateService{
		installation: cliupdate.Installation{
			Kind:         cliupdate.InstallKindHomebrew,
			ResolvedPath: "/opt/homebrew/Caskroom/codefly/1.2.3/codefly",
		},
	}
	var output bytes.Buffer
	if err := runUpdate(context.Background(), &output, &bytes.Buffer{}, service, releaseupdate.ChannelStable, true, false); err != nil {
		t.Fatal(err)
	}
	if service.checked || service.applied {
		t.Fatal("managed installation reached the direct updater")
	}
	if !strings.Contains(output.String(), "brew upgrade --cask") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunUpdateAppliesConfirmedDirectUpdate(t *testing.T) {
	service := &fakeUpdateService{
		installation: cliupdate.Installation{
			Kind:         cliupdate.InstallKindDirect,
			ResolvedPath: "/usr/local/bin/codefly",
		},
		result: cliupdate.CheckResult{
			Current:   "1.2.3",
			Latest:    "1.3.0",
			Available: true,
			Channel:   "stable",
		},
		applyResult: releaseupdate.ApplyResult{Applied: true},
	}
	var output bytes.Buffer
	if err := runUpdate(context.Background(), &output, &bytes.Buffer{}, service, releaseupdate.ChannelStable, true, true); err != nil {
		t.Fatal(err)
	}
	if !service.checked || !service.downgrade || !service.applied {
		t.Fatalf("checked=%v downgrade=%v applied=%v", service.checked, service.downgrade, service.applied)
	}
	if !strings.Contains(output.String(), "Updated Codefly from v1.2.3 to v1.3.0.") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunUpdateRequiresExplicitConsentWithoutTerminal(t *testing.T) {
	service := &fakeUpdateService{
		installation: cliupdate.Installation{Kind: cliupdate.InstallKindDirect},
		result: cliupdate.CheckResult{
			Current:   "1.2.3",
			Latest:    "1.3.0",
			Available: true,
		},
	}
	err := runUpdate(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, service, releaseupdate.ChannelStable, false, false)
	if err == nil || !strings.Contains(err.Error(), "pass --yes") {
		t.Fatalf("error = %v", err)
	}
	if service.applied {
		t.Fatal("update applied without consent")
	}
}

func TestRunUpdateReportsCommittedReplacementWithCleanupError(t *testing.T) {
	service := &fakeUpdateService{
		installation: cliupdate.Installation{Kind: cliupdate.InstallKindDirect},
		result: cliupdate.CheckResult{
			Current:   "1.2.3",
			Latest:    "1.3.0",
			Available: true,
		},
		applyResult: releaseupdate.ApplyResult{
			Applied:      true,
			PreviousPath: "/tmp/codefly-update/previous",
			CleanupPath:  "/tmp/codefly-update",
		},
		applyErr: releaseupdate.ErrApplyCleanup,
	}
	var output, errorOutput bytes.Buffer
	err := runUpdate(context.Background(), &output, &errorOutput, service, releaseupdate.ChannelStable, true, false)
	if !errors.Is(err, releaseupdate.ErrApplyCleanup) {
		t.Fatalf("error = %v, want ErrApplyCleanup", err)
	}
	if !strings.Contains(output.String(), "Updated Codefly") ||
		!strings.Contains(errorOutput.String(), "was installed") ||
		!strings.Contains(errorOutput.String(), service.applyResult.PreviousPath) ||
		!strings.Contains(errorOutput.String(), service.applyResult.CleanupPath) {
		t.Fatalf("stdout = %q, stderr = %q", output.String(), errorOutput.String())
	}
}

func TestRunUpdateReportsRetainedPreviousExecutableAfterRollbackFailure(t *testing.T) {
	service := &fakeUpdateService{
		installation: cliupdate.Installation{Kind: cliupdate.InstallKindDirect},
		result: cliupdate.CheckResult{
			Current:   "1.2.3",
			Latest:    "1.3.0",
			Available: true,
		},
		applyResult: releaseupdate.ApplyResult{
			PreviousPath: "/tmp/codefly-update/previous",
			CleanupPath:  "/tmp/codefly-update",
		},
		applyErr: errors.New("install replacement; restore failed"),
	}
	var errorOutput bytes.Buffer
	err := runUpdate(context.Background(), &bytes.Buffer{}, &errorOutput, service, releaseupdate.ChannelStable, true, false)
	if !errors.Is(err, service.applyErr) {
		t.Fatalf("error = %v, want %v", err, service.applyErr)
	}
	if !strings.Contains(errorOutput.String(), service.applyResult.PreviousPath) ||
		!strings.Contains(errorOutput.String(), service.applyResult.CleanupPath) {
		t.Fatalf("stderr = %q", errorOutput.String())
	}
}

func TestParseChannel(t *testing.T) {
	if got, err := parseChannel(" BETA "); err != nil || got != releaseupdate.ChannelBeta {
		t.Fatalf("parseChannel() = %q, %v", got, err)
	}
	if _, err := parseChannel("nightly"); err == nil {
		t.Fatal("expected unsupported channel error")
	}
}

func TestCheckUpdateCommandDevelopmentJSON(t *testing.T) {
	command := newCheckUpdateCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	var result cliupdate.CheckResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || result.Current != "development" || result.Available {
		t.Fatalf("result = %#v", result)
	}
}

func TestCheckUpdateCommandJSONErrorIsMachineReadable(t *testing.T) {
	command := newCheckUpdateCommand()
	var output, errorOutput bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&errorOutput)
	command.SetArgs([]string{"--channel", "nightly", "--json"})
	err := command.Execute()
	if err == nil {
		t.Fatal("expected unsupported channel error")
	}
	if errorOutput.Len() != 0 {
		t.Fatalf("stderr = %q, want empty machine-readable output", errorOutput.String())
	}
	var marker interface{ MachineReadable() bool }
	if !errors.As(err, &marker) || !marker.MachineReadable() {
		t.Fatalf("error is not machine-readable: %v", err)
	}
	var payload struct {
		SchemaVersion int    `json:"schema_version"`
		Error         string `json:"error"`
	}
	if decodeErr := json.Unmarshal(output.Bytes(), &payload); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if payload.SchemaVersion != 1 || !strings.Contains(payload.Error, "unsupported release channel") {
		t.Fatalf("payload = %#v", payload)
	}
}
