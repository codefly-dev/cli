package cmd

import (
	"context"
	"encoding/json"
	"github.com/codefly-dev/cli/pkg/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigurationEvidenceDigestTracksProducerBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "producer")
	if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := executableDigest(p)
	if err != nil || digest != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("digest=%s err=%v", digest, err)
	}
	if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := executableDigest(p)
	if err != nil || changed == digest {
		t.Fatalf("producer change lost: %v", err)
	}
	if _, err := executableDigest(p + "-missing"); err == nil {
		t.Fatal("missing producer accepted")
	}
}

func TestConfigurationEvidenceCoversFailedCollectionsWithoutClaimingInputs(t *testing.T) {
	report := runReadiness(t, workspaceReadinessOptions{dir: t.TempDir(), sourceOnly: true})
	e := report.ConfigurationEvidence
	if e == nil || len(e.ExecutableSHA256) != 64 || e.InputBinding != "unavailable" || e.Coverage != "composed-group-key-inventory" {
		t.Fatalf("wrong evidence: %+v", e)
	}
	start, err := time.Parse(time.RFC3339Nano, e.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse(time.RFC3339Nano, e.CompletedAt)
	if err != nil || end.Before(start) {
		t.Fatalf("invalid interval: %+v", e)
	}
	if report.ConfigurationResolved {
		t.Fatal("failed workspace reported resolved")
	}
	ordinary := runReadiness(t, workspaceReadinessOptions{dir: t.TempDir()})
	if ordinary.ConfigurationEvidence != nil {
		t.Fatal("source evidence attached to ordinary readiness")
	}
}

func TestConfigurationEvidenceJSONRetainsFalseMarkersFromGeneratedMessages(t *testing.T) {
	report := workspaceReadinessReport{
		SchemaVersion:         1,
		ConfigurationProfiles: []*basev0.ConfigurationProfileSelection{{Location: "../base/configurations", Candidates: []string{"staging"}, Found: false}},
		ConfigurationDecisions: []*basev0.ConfigurationDecision{{Group: "app", Final: false, Rule: basev0.ConfigurationRule_CONFIGURATION_RULE_WORKSPACE_KEY_REPLACES_MODULE_DEFAULT,
			Selected: []*basev0.ConfigurationOrigin{{Group: "app", Key: "URL", File: "../base/app.env", Document: false}}}},
	}
	encoded, err := json.Marshal(&report)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	decision := wire["configuration_decisions"].([]any)[0].(map[string]any)
	if v, ok := decision["final"]; !ok || v != false {
		t.Fatalf("intermediate decision marker lost: %s", encoded)
	}
	if decision["rule"] != "CONFIGURATION_RULE_WORKSPACE_KEY_REPLACES_MODULE_DEFAULT" {
		t.Fatalf("canonical enum lost: %s", encoded)
	}
	origin := decision["selected"].([]any)[0].(map[string]any)
	if v, ok := origin["document"]; !ok || v != false {
		t.Fatalf("key/document marker lost: %s", encoded)
	}
	profile := wire["configuration_profiles"].([]any)[0].(map[string]any)
	if v, ok := profile["found"]; !ok || v != false {
		t.Fatalf("missing profile marker lost: %s", encoded)
	}
	if _, ok := profile["layers"].([]any); !ok {
		t.Fatalf("empty layers must remain an array: %s", encoded)
	}
}

func TestReadinessDoesNotAcquireWorkspaceEvenWithExecutionResolver(t *testing.T) {
	root := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": "name: product\nlayout: modules\nworkspaces:\n  - name: base\n    source: example/missing-inspection-fixture\n    version: '1.2.3'\nenvironments:\n  - name: local\n"})
	cache := filepath.Join(t.TempDir(), "cache")
	t.Setenv(composition.ModuleCacheEnv, cache)
	for _, sourceOnly := range []bool{false, true} {
		report := workspaceReadiness(composition.WithWorkspaceResolver(context.Background()), &workspaceReadinessOptions{dir: root, env: "local", sourceOnly: sourceOnly})
		checks := findDiagnostics(report, codeWorkspaceInvalid)
		if len(checks) != 1 || !strings.Contains(checks[0].Message, "not cached") {
			t.Fatalf("unexpected report: %+v", report)
		}
		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Fatalf("inspection created cache: %v", err)
		}
	}
}

func TestSourceEvidenceResolvedEmptyArraysRemainExplicit(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, map[string]string{"configurations/local/app.env": ""})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, sourceOnly: true})
	if !report.ConfigurationResolved {
		t.Fatalf("not resolved: %+v", report)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"configuration_origins", "configuration_decisions", "configuration_groups", "configuration_profiles"} {
		if _, ok := data[key].([]any); !ok {
			t.Fatalf("%s is not an explicit array: %s", key, raw)
		}
	}
	if len(data["configuration_origins"].([]any)) != 0 {
		t.Fatalf("unexpected origins: %s", raw)
	}
}

func TestDoctorPolicyRereadKeepsInspectionContext(t *testing.T) {
	initial := "name: product\nlayout: modules\nmodules:\n  - name: local\n"
	root := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": initial})
	cache := filepath.Join(t.TempDir(), "cache")
	t.Setenv(composition.ModuleCacheEnv, cache)
	if err := os.WriteFile(filepath.Join(root, "git"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	ctx := composition.WithWorkspaceInspection(context.Background())
	ws, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(initial+"workspaces:\n  - name: later\n    source: example/missing-inspection-fixture\n    version: '1.2.3'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report := &workspaceReadinessReport{}
	checkModuleTrust(ctx, ws, report)
	checks := findDiagnostics(report, codeWorkspaceInvalid)
	if len(checks) != 1 || !strings.Contains(checks[0].Message, "not cached") {
		t.Fatalf("unexpected checks: %+v", report.Checks)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("policy re-read acquired a new import: %v", err)
	}
}
