// Package publish implements `codefly publish` — the unified release
// command for codefly modules and agents. Replaces the per-repo
// scripts/publish/{tag,re_tag,push}.sh that have proliferated across
// codefly-dev — same logic in 16+ places, drift between them, and
// every one of them used `git push -f` against main.
//
// Usage:
//
//	codefly publish                    # patch bump, auto-detect mode
//	codefly publish patch|minor|major  # explicit bump type
//	codefly publish --re-tag           # re-cut existing tag at HEAD
//
// Auto-detection walks cwd looking for one of the known manifest
// files. Each repo has exactly one; multiple is a configuration
// error and surfaces at load time.
//
// The release engine itself mirrors the hardened core/scripts/
// publish/tag.sh: four pre-flight gates (working tree clean, on main,
// in sync with origin, tag doesn't already exist), bump version,
// commit, tag, push — never with --force on either main or the tag.
package publish

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Masterminds/semver"
	"gopkg.in/yaml.v3"
)

// Mode names the kind of repo we're releasing — different manifest
// shapes ship version metadata in different places, but the release
// flow is identical.
type Mode string

const (
	// ModeAgent — service-agent or module-agent repo. Bumps
	// agent.codefly.yaml. Beyond tagging, agent publishes run
	// release-grade CI and upload the loader-compatible release assets
	// (darwin/arm64 + linux/amd64 archives and SBOMs) that
	// `codefly agent install` downloads — see cmd/publish/agent_release.go.
	ModeAgent Mode = "agent"
	// ModeCoreModule — the codefly core repo. Bumps version/info.codefly.yaml.
	ModeCoreModule Mode = "core-module"
	// ModeCLI — the codefly cli repo. Bumps pkg/cli/info.yaml.
	ModeCLI Mode = "cli"
	// ModeStandaloneModule — sdk-go and similar Go modules with a
	// flat info.codefly.yaml. Bumps that file.
	ModeStandaloneModule Mode = "standalone-module"
)

// Manifest is the resolved manifest for the current working dir.
// All four Mode variants reduce to the same minimal interface: a
// path to a YAML file and a version string inside it.
type Manifest struct {
	// Mode tells the release engine which repo shape we're in. Only
	// surfaces in diagnostics today; reserved for per-mode behavior
	// later (e.g. agent-specific GoReleaser checks).
	Mode Mode

	// Path is the absolute path to the manifest YAML.
	Path string

	// Version is the parsed semver version. Always set after
	// successful Detect. Mutated by Bump.
	Version *semver.Version
}

// candidate enumerates the (relative-path → mode) lookups Detect
// performs. Order is stable so detection is deterministic when
// multiple files happen to coexist (which would itself be a config
// error — caught below).
var candidates = []struct {
	relPath string
	mode    Mode
}{
	{"agent.codefly.yaml", ModeAgent},
	{"version/info.codefly.yaml", ModeCoreModule},
	{"pkg/cli/info.yaml", ModeCLI},
	{"info.codefly.yaml", ModeStandaloneModule},
}

// Detect walks the given directory looking for one of the known
// manifests. Returns the first match in candidate-order.
//
// Multiple manifests in the same dir is an error — the operator
// must declare which one is canonical. We don't silently pick;
// silent picking has been the source of every script-vs-script
// drift bug in the existing publish flows.
func Detect(dir string) (*Manifest, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}

	var matches []*Manifest
	for _, c := range candidates {
		path := filepath.Join(abs, c.relPath)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		ver, err := readVersion(path)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", path, err)
		}
		matches = append(matches, &Manifest{Mode: c.mode, Path: path, Version: ver})
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no codefly manifest in %s; expected one of: agent.codefly.yaml | version/info.codefly.yaml | pkg/cli/info.yaml | info.codefly.yaml", abs)
	case 1:
		return matches[0], nil
	default:
		paths := make([]string, len(matches))
		for i, m := range matches {
			paths[i] = m.Path
		}
		return nil, fmt.Errorf("multiple manifests found in %s — configuration error: %v", abs, paths)
	}
}

// readVersion parses the YAML's `version` field. We expect every
// manifest shape to share the same top-level field name regardless
// of whatever else they carry.
func readVersion(path string) (*semver.Version, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is composed from a caller-trusted dir
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	var probe struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if probe.Version == "" {
		return nil, fmt.Errorf("manifest has no `version` field (or it is empty)")
	}
	v, err := semver.NewVersion(probe.Version)
	if err != nil {
		return nil, fmt.Errorf("invalid semver %q: %w", probe.Version, err)
	}
	return v, nil
}

// Bump returns a NEW version with the requested component incremented.
// We don't mutate the receiver's Version pointer — Bump is a pure
// computation; the caller decides when to write the new value back.
//
// bumpType: "patch", "minor", "major". Default (empty) is "patch".
func (m *Manifest) Bump(bumpType string) (*semver.Version, error) {
	if bumpType == "" {
		bumpType = "patch"
	}
	switch bumpType {
	case "patch":
		next := m.Version.IncPatch()
		return &next, nil
	case "minor":
		next := m.Version.IncMinor()
		return &next, nil
	case "major":
		next := m.Version.IncMajor()
		return &next, nil
	default:
		return nil, fmt.Errorf("invalid bump type %q (use patch | minor | major)", bumpType)
	}
}

// WriteVersion replaces the `version:` line in the manifest with the
// new value, preserving every other byte of the file (YAML parsers
// would re-format the doc on round-trip and obliterate hand-tuned
// comment placement). Targets the SAME line-anchor pattern the
// existing tag.sh sed used, so it produces byte-for-byte identical
// output for the same input — making the migration a no-op diff.
func (m *Manifest) WriteVersion(newVer *semver.Version) error {
	raw, err := os.ReadFile(m.Path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read %s: %w", m.Path, err)
	}
	updated, err := replaceVersionLine(raw, newVer.String())
	if err != nil {
		return fmt.Errorf("rewrite version: %w", err)
	}
	// 0o600 mirrors the file mode YAML manifests are typically
	// created with; we read+write so an existing wider mode (e.g.
	// 0o644) gets normalized — that's intentional, the manifest
	// is sensitive enough not to be world-readable.
	if err := os.WriteFile(m.Path, updated, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", m.Path, err)
	}
	m.Version = newVer
	return nil
}

// Tag returns the git tag string for this manifest's current version
// — `v<semver>` by codefly convention (matches every existing tag.sh).
func (m *Manifest) Tag() string {
	return "v" + m.Version.String()
}
