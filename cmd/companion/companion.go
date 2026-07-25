// Package companion implements `codefly companion` subcommands: build,
// push, list. Replaces the hand-rolled bash scripts in
// core/companions/scripts/ + core/companions/<name>/scripts/ with a
// typed Go implementation that codefly's dependency graph and
// permission system can reason about.
//
// Why move from bash to Go:
//   - Discoverability: `codefly companion --help` lists what's
//     available; the bash scripts were findable only by reading the
//     core/ tree.
//   - Type safety on companion metadata: info.codefly.yaml is parsed
//     once with a real schema rather than yq-extracted strings.
//   - Composition with the rest of codefly: errors flow through the
//     CLI's pkg/cli helpers, output is the same color-coded format
//     the rest of the CLI uses, hooks like --push or --tag-bump
//     compose without writing more bash.
//   - Toolbox-architecture alignment: this is the host CLI's
//     companion-management surface, parallel to `codefly agent`
//     for agents and (eventually) `codefly toolbox` for toolboxes.
package companion

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CompanionInfo mirrors the fields read from <companion>/info.codefly.yaml.
// We only need version today; future fields (description, base image,
// build args) extend this struct without breaking callers.
type CompanionInfo struct {
	// Version is the image tag that gets applied: codeflydev/<name>:<version>.
	Version string `yaml:"version"`
}

// Companion describes a discovered companion: its directory, name, and
// parsed info. Keeps the build/push commands decoupled from filesystem
// scanning.
type Companion struct {
	// Name is the companion's directory basename — also the suffix in
	// the resulting image tag (codeflydev/<Name>:<Info.Version>).
	Name string
	// Dir is the absolute path to the companion directory.
	Dir string
	// Info is the parsed info.codefly.yaml.
	Info CompanionInfo
	// HasFlake is true when a flake.nix exists alongside the
	// Dockerfile. The proto companion is the pilot for nix-built
	// images; we prefer the flake path when it's present AND nix is
	// installed on the host.
	HasFlake bool
	// HasDockerfile is true when a Dockerfile exists. Most companions
	// have one; nix-only companions (future) won't.
	HasDockerfile bool
}

// LoadCompanion reads info.codefly.yaml at dir and returns a
// Companion descriptor. Returns an error if the manifest is missing
// or unparseable — the caller decides whether to skip or abort.
func LoadCompanion(dir string) (*Companion, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	manifest := filepath.Join(abs, "info.codefly.yaml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifest, err)
	}
	var info CompanionInfo
	if err := yaml.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifest, err)
	}
	if info.Version == "" {
		return nil, fmt.Errorf("%s: version field is required", manifest)
	}

	c := &Companion{
		Name: filepath.Base(abs),
		Dir:  abs,
		Info: info,
	}
	if _, err := os.Stat(filepath.Join(abs, "flake.nix")); err == nil {
		c.HasFlake = true
	}
	if _, err := os.Stat(filepath.Join(abs, "Dockerfile")); err == nil {
		c.HasDockerfile = true
	}
	return c, nil
}

// FindCompanionsRoot walks upward from start looking for a directory
// that contains companions/. Returns the path to the parent (i.e.
// core/) — the companions/ dir is at <root>/companions. This mirrors
// the behavior of build_companions.sh which assumes it runs from
// core/.
//
// Returns the original start if no companions/ ancestor is found, and
// the caller's existence check on filepath.Join(root, "companions")
// will fail loudly. Letting the caller emit the error keeps this
// function pure / cheap.
func FindCompanionsRoot(start string) string {
	cur, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for {
		candidate := filepath.Join(cur, "companions")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return start
		}
		cur = parent
	}
}

// ListCompanions discovers every companion directory under root/companions/.
// A directory counts as a companion if it has an info.codefly.yaml.
// Order is sorted by name for deterministic output. Returns nil + nil
// when no companions/ exists at root.
func ListCompanions(root string) ([]*Companion, error) {
	companionsDir := filepath.Join(root, "companions")
	entries, err := os.ReadDir(companionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read companions dir %s: %w", companionsDir, err)
	}

	out := make([]*Companion, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip dirs that don't have info.codefly.yaml (e.g. shared
		// scripts/ subdirectory). LoadCompanion's read of the manifest
		// is the authoritative test.
		c, err := LoadCompanion(filepath.Join(companionsDir, e.Name()))
		if err != nil {
			// Quietly skip non-companion dirs (no info.codefly.yaml).
			// We can't distinguish "missing" from "malformed" without
			// inspecting the error type, but malformed manifests will
			// fail loudly when the user invokes build/push directly
			// against that companion — which is the right time to
			// surface the parse error.
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Tag returns the image tag this companion will produce: codeflydev/<name>:<version>.
func (c *Companion) Tag() string {
	return fmt.Sprintf("codeflydev/%s:%s", c.Name, c.Info.Version)
}

// ProducesImage reports whether this companion builds a Docker image. A
// directory with an info.codefly.yaml but neither a Dockerfile nor a
// flake.nix (e.g. the `golang` Go-package companion) is a real companion
// but has no image to publish or verify.
func (c *Companion) ProducesImage() bool {
	return c.HasDockerfile || c.HasFlake
}
