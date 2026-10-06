package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type fixtureManifest struct {
	Schema string        `json:"schema"`
	Cases  []fixtureCase `json:"cases"`
}
type fixtureCase struct {
	Name      string  `json:"name"`
	Input     string  `json:"input"`
	Context   string  `json:"context"`
	Outcome   string  `json:"outcome"`
	RuleID    *string `json:"rule_id"`
	Canonical *string `json:"canonical"`
	Digest    *string `json:"digest"`
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestConformanceData(t *testing.T) {
	var manifest fixtureManifest
	if err := json.Unmarshal(read(t, "testdata/manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, f := range manifest.Cases {
		t.Run(f.Name, func(t *testing.T) {
			inventory := read(t, filepath.Join("testdata", f.Input))
			context := read(t, filepath.Join("testdata", f.Context))
			result, err := Validate(inventory, context)
			if f.Outcome == "invalid" {
				var v *Violation
				if !errors.As(err, &v) || f.RuleID == nil || v.Rule != *f.RuleID {
					t.Fatalf("got %v, want rule %s", err, valueOrEmpty(f.RuleID))
				}
				if result != nil {
					t.Fatal("refusal returned usable result")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := read(t, filepath.Join("testdata", *f.Canonical))
			if !bytes.Equal(result.Canonical(), expected) || result.Digest() != *f.Digest {
				t.Fatal("canonical bytes or digest differ from data oracle")
			}
			if err = CheckSchema(inventory); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRetainedBytesAndProjection(t *testing.T) {
	input := read(t, "testdata/generated/authenticator-is-second.input.json")
	ctx := read(t, "testdata/generated/authenticator-is-second.context.json")
	result, err := Validate(input, ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := result.Digest()
	for i := range input {
		input[i] = 0
	}
	for i := range ctx {
		ctx[i] = 0
	}
	row := result.Rows()[0]
	if row.Container == nil || *row.Container != "worker" || len(row.Images) != 4 || row.App[0] != "helper" || row.Init[0] != "prepare" {
		t.Fatalf("lost authoritative roles: %+v", row)
	}
	raw, _ := json.Marshal(row)
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	if len(keys) != 7 {
		t.Fatal("projection must have exactly seven fields")
	}
	row.Images["worker"] = "tampered"
	row.App[0] = "tampered"
	*row.Container = "tampered"
	inv := result.Inventory()
	inv.Workloads[0].Template.Spec.Containers[0].Image = "tampered"
	returned := result.Canonical()
	returned[0] = 0
	if result.Digest() != before || *result.Rows()[0].Container != "worker" || result.Rows()[0].Images["worker"] == "tampered" {
		t.Fatal("caller mutation changed retained result")
	}
}
func TestPythonEntryPoint(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "deployment-contract")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deployment-contract")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	run := exec.Command("python3", "python/conformance.py", binary)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	} else {
		t.Log(string(output))
	}
}

func TestEveryLibraryRuleHasConformanceData(t *testing.T) {
	var manifest fixtureManifest
	if err := json.Unmarshal(read(t, "testdata/manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, fixture := range manifest.Cases {
		if fixture.RuleID != nil {
			covered[*fixture.RuleID] = true
		}
	}
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	catalogue := string(read(t, "RULES.md"))
	pattern := regexp.MustCompile(`refuse\("([A-Z_]+)"`)
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		for _, match := range pattern.FindAllSubmatch(read(t, source), -1) {
			rule := string(match[1])
			if !covered[rule] {
				t.Errorf("%s has no refusal fixture", rule)
			}
			if !strings.Contains(catalogue, "`"+rule+"`") {
				t.Errorf("%s is absent from the frozen rule catalogue", rule)
			}
		}
	}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
