package sourceworkspace

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	"github.com/codefly-dev/core/resources"
)

const CompatibilityRosterRelativePath = "pkg/sourceworkspace/compatibility.json"

//go:embed compatibility.json
var embeddedCompatibilityRoster []byte

type CompatibilityRoster struct {
	SchemaVersion int                   `json:"schema_version"`
	Plugins       []PluginCompatibility `json:"plugins"`
}

type PluginCompatibility struct {
	Publisher  string   `json:"publisher"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Markers    []string `json:"markers,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Fallback   bool     `json:"fallback,omitempty"`
}

type SelectionEvidence struct {
	Kind  string
	Value string
}

func (e SelectionEvidence) String() string {
	if e.Value == "" {
		return e.Kind
	}
	return e.Kind + ":" + e.Value
}

var compatibilityRoster = mustParseCompatibilityRoster(embeddedCompatibilityRoster)

var (
	GenericGoPluginVersion     = mustPinnedVersion("codefly.dev", "go")
	GenericPythonPluginVersion = mustPinnedVersion("codefly.dev", "python")
	GenericPluginVersion       = mustPinnedVersion("codefly.dev", "generic")
	NodePluginVersion          = mustPinnedVersion("codefly.dev", "nextjs")
	RustPluginVersion          = mustPinnedVersion("codefly.dev", "rust")
	SwiftPluginVersion         = mustPinnedVersion("codefly.dev", "swift")
)

func mustParseCompatibilityRoster(payload []byte) CompatibilityRoster {
	roster, err := ParseCompatibilityRoster(payload)
	if err != nil {
		panic(err)
	}
	return roster
}

func mustPinnedVersion(publisher, name string) string {
	plugin, ok := compatibilityRoster.Plugin(publisher, name)
	if !ok {
		panic(fmt.Sprintf("source-workspace compatibility roster has no %s/%s plugin", publisher, name))
	}
	return plugin.Version
}

func ParseCompatibilityRoster(payload []byte) (CompatibilityRoster, error) {
	var roster CompatibilityRoster
	if err := json.Unmarshal(payload, &roster); err != nil {
		return CompatibilityRoster{}, fmt.Errorf("parse source-workspace compatibility roster: %w", err)
	}
	if err := roster.Validate(); err != nil {
		return CompatibilityRoster{}, err
	}
	return roster, nil
}

func LoadCompatibilityRoster(path string) (CompatibilityRoster, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return CompatibilityRoster{}, fmt.Errorf("read source-workspace compatibility roster: %w", err)
	}
	return ParseCompatibilityRoster(payload)
}

func WriteCompatibilityRoster(path string, roster CompatibilityRoster) error {
	if err := roster.Validate(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(roster, "", "  ")
	if err != nil {
		return fmt.Errorf("encode source-workspace compatibility roster: %w", err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write source-workspace compatibility roster: %w", err)
	}
	return nil
}

func Roster() CompatibilityRoster {
	roster := compatibilityRoster
	roster.Plugins = append([]PluginCompatibility(nil), compatibilityRoster.Plugins...)
	for i := range roster.Plugins {
		roster.Plugins[i].Markers = append([]string(nil), roster.Plugins[i].Markers...)
		roster.Plugins[i].Extensions = append([]string(nil), roster.Plugins[i].Extensions...)
	}
	return roster
}

func PinnedPlugin(publisher, name string) (PluginCompatibility, bool) {
	return compatibilityRoster.Plugin(publisher, name)
}

func (r CompatibilityRoster) Plugin(publisher, name string) (PluginCompatibility, bool) {
	for _, plugin := range r.Plugins {
		if plugin.Publisher == publisher && plugin.Name == name {
			return plugin, true
		}
	}
	return PluginCompatibility{}, false
}

func (p *PluginCompatibility) Agent() *resources.Agent {
	return &resources.Agent{
		Kind:      resources.ServiceAgent,
		Publisher: p.Publisher,
		Name:      p.Name,
		Version:   p.Version,
	}
}

func (r CompatibilityRoster) Validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("source-workspace compatibility roster schema_version = %d, want 1", r.SchemaVersion)
	}
	if len(r.Plugins) == 0 {
		return fmt.Errorf("source-workspace compatibility roster has no plugins")
	}
	agents := map[string]bool{}
	markers := map[string]bool{}
	extensions := map[string]bool{}
	fallbacks := 0
	for i, plugin := range r.Plugins {
		identity := plugin.Publisher + "/" + plugin.Name
		if strings.TrimSpace(plugin.Publisher) == "" || strings.TrimSpace(plugin.Name) == "" {
			return fmt.Errorf("source-workspace compatibility plugin %d must have publisher and name", i)
		}
		if agents[identity] {
			return fmt.Errorf("source-workspace compatibility roster repeats plugin %s", identity)
		}
		agents[identity] = true
		if _, err := semver.Parse(strings.TrimPrefix(plugin.Version, "v")); err != nil || strings.HasPrefix(plugin.Version, "v") {
			return fmt.Errorf("source-workspace compatibility plugin %s has invalid exact version %q", identity, plugin.Version)
		}
		if plugin.Fallback {
			fallbacks++
			if len(plugin.Markers) > 0 || len(plugin.Extensions) > 0 {
				return fmt.Errorf("source-workspace fallback plugin %s cannot declare markers or extensions", identity)
			}
		}
		for _, marker := range plugin.Markers {
			if marker == "" || filepath.IsAbs(marker) || filepath.Clean(marker) != marker || strings.HasPrefix(marker, "..") {
				return fmt.Errorf("source-workspace compatibility plugin %s has invalid marker %q", identity, marker)
			}
			if markers[marker] {
				return fmt.Errorf("source-workspace compatibility roster repeats marker %q", marker)
			}
			markers[marker] = true
		}
		for _, extension := range plugin.Extensions {
			if extension == "" || extension != strings.ToLower(extension) || !strings.HasPrefix(extension, ".") {
				return fmt.Errorf("source-workspace compatibility plugin %s has invalid extension %q", identity, extension)
			}
			if extensions[extension] {
				return fmt.Errorf("source-workspace compatibility roster repeats extension %q", extension)
			}
			extensions[extension] = true
		}
	}
	if fallbacks != 1 {
		return fmt.Errorf("source-workspace compatibility roster must have exactly one fallback plugin")
	}
	if !r.Plugins[len(r.Plugins)-1].Fallback {
		return fmt.Errorf("source-workspace compatibility fallback plugin must be last")
	}
	return nil
}

func (r CompatibilityRoster) SelectPlugin(sourceDir string) (*resources.Agent, SelectionEvidence, error) {
	for _, plugin := range r.Plugins {
		for _, marker := range plugin.Markers {
			if _, err := os.Stat(filepath.Join(sourceDir, marker)); err == nil {
				return plugin.Agent(), SelectionEvidence{Kind: "marker", Value: marker}, nil
			} else if !os.IsNotExist(err) {
				return nil, SelectionEvidence{}, fmt.Errorf("inspect %s source marker: %w", marker, err)
			}
		}
	}

	pluginsByExtension := map[string]PluginCompatibility{}
	for _, plugin := range r.Plugins {
		for _, extension := range plugin.Extensions {
			pluginsByExtension[extension] = plugin
		}
	}
	var selected *resources.Agent
	var evidence SelectionEvidence
	err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != sourceDir && skipDetectionDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if plugin, ok := pluginsByExtension[extension]; ok {
			selected = plugin.Agent()
			evidence = SelectionEvidence{Kind: "extension", Value: extension}
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, SelectionEvidence{}, fmt.Errorf("inspect source files: %w", err)
	}
	if selected != nil {
		return selected, evidence, nil
	}
	for _, plugin := range r.Plugins {
		if plugin.Fallback {
			return plugin.Agent(), SelectionEvidence{Kind: "fallback"}, nil
		}
	}
	panic("validated source-workspace compatibility roster has no fallback")
}
