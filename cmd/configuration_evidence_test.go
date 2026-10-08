package cmd

import (
	"encoding/json"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"os"
	"path/filepath"
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
		ConfigurationProfiles: []*basev0.ConfigurationProfileSelection{{Location: "/missing", Candidates: []string{"staging"}, Found: false}},
		ConfigurationDecisions: []*basev0.ConfigurationDecision{{Group: "app", Final: false,
			Selected: []*basev0.ConfigurationOrigin{{Group: "app", Key: "URL", File: "/base/app.env", Document: false}}}},
	}
	encoded, err := json.Marshal(report)
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
