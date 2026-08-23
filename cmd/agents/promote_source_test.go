package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/resources"
)

func fixturePromotionRoster(t *testing.T) string {
	t.Helper()
	cliDir := t.TempDir()
	rosterPath := filepath.Join(cliDir, filepath.FromSlash(sourceworkspace.CompatibilityRosterRelativePath))
	if err := os.MkdirAll(filepath.Dir(rosterPath), 0o755); err != nil {
		t.Fatal(err)
	}
	roster := sourceworkspace.CompatibilityRoster{
		SchemaVersion: 1,
		Plugins: []sourceworkspace.PluginCompatibility{
			{
				Publisher:  "codefly.dev",
				Name:       "python",
				Version:    "1.2.3",
				Markers:    []string{"pyproject.toml", "requirements.txt"},
				Extensions: []string{".py"},
			},
			{Publisher: "codefly.dev", Name: "generic", Version: "1.0.0", Fallback: true},
		},
	}
	if err := sourceworkspace.WriteCompatibilityRoster(rosterPath, roster); err != nil {
		t.Fatal(err)
	}
	return cliDir
}

func markerFixture(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPromoteSourcePluginQualifiesEveryMarkerBeforeUpdatingRoster(t *testing.T) {
	cliDir := fixturePromotionRoster(t)
	rosterPath := filepath.Join(cliDir, filepath.FromSlash(sourceworkspace.CompatibilityRosterRelativePath))
	before, err := sourceworkspace.LoadCompatibilityRoster(rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"pyproject.toml":   markerFixture(t, "pyproject.toml"),
		"requirements.txt": markerFixture(t, "requirements.txt"),
	}
	var calls []string
	qualify := func(_ context.Context, agent *resources.Agent, marker, fixture string) error {
		if agent.Identifier() != "codefly.dev/python:1.2.4" {
			t.Fatalf("qualified agent = %s, want exact candidate", agent.Identifier())
		}
		calls = append(calls, marker+"="+fixture)
		return nil
	}

	result, err := promoteSourcePlugin(context.Background(), sourcePromotionOptions{
		agentSpec: "codefly.dev/python:1.2.4",
		cliDir:    cliDir,
		fixtures:  fixtures,
	}, qualify)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"pyproject.toml=" + fixtures["pyproject.toml"],
		"requirements.txt=" + fixtures["requirements.txt"],
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("qualification calls = %v, want %v", calls, wantCalls)
	}
	if result.previousVersion != "1.2.3" || result.version != "1.2.4" || len(result.proofs) != 2 {
		t.Fatalf("promotion result = %+v", result)
	}
	roster, err := sourceworkspace.LoadCompatibilityRoster(result.rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	plugin, ok := roster.Plugin("codefly.dev", "python")
	if !ok || plugin.Version != "1.2.4" {
		t.Fatalf("promoted plugin = %+v, found = %v", plugin, ok)
	}

	selected, _, err := roster.SelectPlugin(fixtures["pyproject.toml"])
	if err != nil {
		t.Fatal(err)
	}
	if selected.Version != "1.2.4" {
		t.Fatalf("source checkout launches %s, want promoted 1.2.4", selected.Version)
	}
	before.Plugins[0].Version = "1.2.4"
	if !reflect.DeepEqual(roster, before) {
		t.Fatalf("promotion changed fields beyond the target pin:\n before=%+v\n after=%+v", before, roster)
	}
}

func TestPromoteSourcePluginDoesNotUpdateRosterWhenQualificationFails(t *testing.T) {
	cliDir := fixturePromotionRoster(t)
	fixtures := map[string]string{
		"pyproject.toml":   markerFixture(t, "pyproject.toml"),
		"requirements.txt": markerFixture(t, "requirements.txt"),
	}
	qualificationFailure := errors.New("capability handshake failed")
	_, err := promoteSourcePlugin(context.Background(), sourcePromotionOptions{
		agentSpec: "codefly.dev/python:1.2.4",
		cliDir:    cliDir,
		fixtures:  fixtures,
	}, func(_ context.Context, _ *resources.Agent, marker, _ string) error {
		if marker == "requirements.txt" {
			return qualificationFailure
		}
		return nil
	})
	if !errors.Is(err, qualificationFailure) {
		t.Fatalf("promotion error = %v, want qualification failure", err)
	}
	roster, loadErr := sourceworkspace.LoadCompatibilityRoster(filepath.Join(cliDir, filepath.FromSlash(sourceworkspace.CompatibilityRosterRelativePath)))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	plugin, _ := roster.Plugin("codefly.dev", "python")
	if plugin.Version != "1.2.3" {
		t.Fatalf("pin changed after failed qualification: %s", plugin.Version)
	}
}

func TestPromoteSourcePluginRequiresEveryOwnedMarkerAndNewerVersion(t *testing.T) {
	cliDir := fixturePromotionRoster(t)
	fixture := markerFixture(t, "pyproject.toml")
	never := func(context.Context, *resources.Agent, string, string) error {
		t.Fatal("qualification should not run")
		return nil
	}

	_, err := promoteSourcePlugin(context.Background(), sourcePromotionOptions{
		agentSpec: "codefly.dev/python:1.2.4",
		cliDir:    cliDir,
		fixtures:  map[string]string{"pyproject.toml": fixture},
	}, never)
	if err == nil {
		t.Fatal("promotion with a missing marker fixture was accepted")
	}

	_, err = promoteSourcePlugin(context.Background(), sourcePromotionOptions{
		agentSpec: "codefly.dev/python:1.2.3",
		cliDir:    cliDir,
		fixtures: map[string]string{
			"pyproject.toml":   fixture,
			"requirements.txt": markerFixture(t, "requirements.txt"),
		},
	}, never)
	if err == nil {
		t.Fatal("promotion to the current pin was accepted")
	}
}

func TestPromoteSourcePluginRequiresFixtureToSelectThroughNamedMarker(t *testing.T) {
	cliDir := fixturePromotionRoster(t)
	requirementsFixture := markerFixture(t, "requirements.txt")
	if err := os.WriteFile(filepath.Join(requirementsFixture, "pyproject.toml"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qualified := 0
	_, err := promoteSourcePlugin(context.Background(), sourcePromotionOptions{
		agentSpec: "codefly.dev/python:1.2.4",
		cliDir:    cliDir,
		fixtures: map[string]string{
			"pyproject.toml":   markerFixture(t, "pyproject.toml"),
			"requirements.txt": requirementsFixture,
		},
	}, func(context.Context, *resources.Agent, string, string) error {
		qualified++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "marker:pyproject.toml") {
		t.Fatalf("promotion error = %v, want conflicting marker evidence", err)
	}
	if qualified != 1 {
		t.Fatalf("qualified fixtures = %d, want only the valid first marker", qualified)
	}
}

func TestParseSourcePromotionFixturesRejectsDuplicates(t *testing.T) {
	if _, err := parseSourcePromotionFixtures([]string{"go.mod=/one", "go.mod=/two"}); err == nil {
		t.Fatal("duplicate fixture marker was accepted")
	}
	fixtures, err := parseSourcePromotionFixtures([]string{"go.mod=/fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if fixtures["go.mod"] != "/fixture" {
		t.Fatalf("fixtures = %v", fixtures)
	}
}
