package sourceworkspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedCompatibilityRosterDrivesExportedPins(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{name: "go", version: GenericGoPluginVersion},
		{name: "python", version: GenericPythonPluginVersion},
		{name: "generic", version: GenericPluginVersion},
		{name: "nextjs", version: NodePluginVersion},
		{name: "rust", version: RustPluginVersion},
		{name: "swift", version: SwiftPluginVersion},
	}
	for _, test := range tests {
		plugin, ok := PinnedPlugin("codefly.dev", test.name)
		if !ok {
			t.Fatalf("missing codefly.dev/%s", test.name)
		}
		if plugin.Version != test.version {
			t.Fatalf("codefly.dev/%s version = %q, exported pin = %q", test.name, plugin.Version, test.version)
		}
	}
}

func TestCompatibilityRosterSelectionReportsEvidence(t *testing.T) {
	tests := []struct {
		file     string
		name     string
		evidence string
	}{
		{file: "go.mod", name: "go", evidence: "marker:go.mod"},
		{file: "requirements.txt", name: "python", evidence: "marker:requirements.txt"},
		{file: "main.ts", name: "nextjs", evidence: "extension:.ts"},
		{file: "main.rs", name: "rust", evidence: "extension:.rs"},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, test.file), []byte("fixture\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			plugin, evidence, err := Roster().SelectPlugin(dir)
			if err != nil {
				t.Fatal(err)
			}
			if plugin.Name != test.name || evidence.String() != test.evidence {
				t.Fatalf("selection = %s via %s, want %s via %s", plugin.Name, evidence, test.name, test.evidence)
			}
		})
	}
}

func TestParseCompatibilityRosterRejectsFloatingAndAmbiguousEntries(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name: "floating version",
			payload: `{"schema_version":1,"plugins":[
                {"publisher":"codefly.dev","name":"go","version":"latest","markers":["go.mod"]},
                {"publisher":"codefly.dev","name":"generic","version":"0.0.1","fallback":true}
            ]}`,
		},
		{
			name: "duplicate marker",
			payload: `{"schema_version":1,"plugins":[
                {"publisher":"codefly.dev","name":"go","version":"0.0.1","markers":["project"]},
                {"publisher":"codefly.dev","name":"python","version":"0.0.1","markers":["project"]},
                {"publisher":"codefly.dev","name":"generic","version":"0.0.1","fallback":true}
            ]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseCompatibilityRoster([]byte(test.payload)); err == nil {
				t.Fatal("invalid roster was accepted")
			}
		})
	}
}

func TestWriteCompatibilityRosterPromotesSelectionPin(t *testing.T) {
	roster := Roster()
	for i := range roster.Plugins {
		if roster.Plugins[i].Name == "go" {
			roster.Plugins[i].Version = "9.9.9"
		}
	}
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := WriteCompatibilityRoster(path, roster); err != nil {
		t.Fatal(err)
	}
	written, err := LoadCompatibilityRoster(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin, _, err := written.SelectPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Version != "9.9.9" {
		t.Fatalf("selected version = %q, want promoted 9.9.9", plugin.Version)
	}
}
