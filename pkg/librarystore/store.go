// Package librarystore publishes and resolves codefly libraries as durable,
// versioned artifacts consumable by a language's native package manager
// (go get / pip / npm), so a consumer needs neither the codefly toolchain nor
// local source. The Store interface is backend-agnostic; the GitHub-backed
// implementation is the first (and, for now, only) backend.
package librarystore

import (
	"context"
	"fmt"
	"strings"

	"github.com/Masterminds/semver"
)

// Language identifies a library's language export.
type Language string

const (
	LanguageGo         Language = "go"
	LanguagePython     Language = "python"
	LanguageTypeScript Language = "typescript"
)

// Coordinates identify one language export of a library at a semantic version.
type Coordinates struct {
	Language Language
	Name     string
	// Version is a semantic version. Callers may pass it with or without a
	// leading "v"; every Published carries it in canonical form without one.
	Version string
}

// Published is a resolved, durable location for a library export that a native
// package manager can consume.
type Published struct {
	Coordinates
	// ImportPath is the identity the native tool uses: a Go module path, a pip
	// distribution name, or an npm @scope/name.
	ImportPath string
	// Ref is the immutable anchor the version resolves to — a git commit — so a
	// moved tag is detectable.
	Ref string
	// Location is the backing URL.
	Location string
	// Digest is a content hash over the published artifact ("<algorithm>:<hex-or-base64>"),
	// set by Publish from the source tree and by Resolve from the published
	// content at the resolved version. The two agree for a store-published
	// version. The algorithm is backend-specific: the GitHub-backed stores use
	// "sha256"; the npm store uses whatever algorithm the registry's tarball
	// integrity reports (typically "sha512").
	Digest string
	// InstallHint is a copy-pasteable native install command.
	InstallHint string
}

// StoreConfig configures the per-language store backends. It mirrors a
// workspace's `libraries.publish` configuration block.
type StoreConfig struct {
	// GoOwner is the GitHub owner Go exports publish under: github.com/<GoOwner>/<name>-go.
	GoOwner string
	// NpmRegistry is the npm-compatible registry TypeScript exports publish to.
	NpmRegistry string
	// NpmScope is the npm scope (including the leading "@") TypeScript exports
	// publish under: <NpmScope>/<name>.
	NpmScope string
	// PythonOwner is the GitHub owner Python exports publish under:
	// github.com/<PythonOwner>/<name>-python.
	PythonOwner string
}

// NewStoreFor returns the Store backend for language, configured from cfg.
func NewStoreFor(language Language, cfg StoreConfig) (Store, error) {
	switch language {
	case LanguageGo:
		if cfg.GoOwner == "" {
			return nil, fmt.Errorf("librarystore: no GitHub owner configured for go libraries (workspace libraries.publish.go.owner)")
		}
		return NewGitHubStore(cfg.GoOwner), nil
	case LanguagePython:
		if cfg.PythonOwner == "" {
			return nil, fmt.Errorf("librarystore: no GitHub owner configured for python libraries (workspace libraries.publish.python.owner)")
		}
		return NewGitHubStore(cfg.PythonOwner), nil
	case LanguageTypeScript:
		if cfg.NpmRegistry == "" || cfg.NpmScope == "" {
			return nil, fmt.Errorf("librarystore: no npm registry/scope configured for typescript libraries (workspace libraries.publish.typescript)")
		}
		return NewNpmStore(cfg.NpmRegistry, cfg.NpmScope), nil
	default:
		return nil, fmt.Errorf("librarystore: unsupported language %q", language)
	}
}

// PreviewIdentity returns the import path and install hint the store
// NewStoreFor(language, cfg) would publish name@version under, without making
// any network call. It is what `codefly publish library --dry-run` shows, and
// what a caller compares a library manifest's declared export identity
// against before publishing anything.
func PreviewIdentity(language Language, cfg StoreConfig, name, version string) (importPath, installHint string, err error) {
	v, err := semver.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return "", "", fmt.Errorf("librarystore: %q is not a semantic version: %w", version, err)
	}
	tag := versionTag(v.String())
	switch language {
	case LanguageGo:
		if cfg.GoOwner == "" {
			return "", "", fmt.Errorf("librarystore: no GitHub owner configured for go libraries (workspace libraries.publish.go.owner)")
		}
		importPath = fmt.Sprintf("github.com/%s/%s", cfg.GoOwner, repositoryName(language, name))
		return importPath, fmt.Sprintf("go get %s@%s", importPath, tag), nil
	case LanguagePython:
		if cfg.PythonOwner == "" {
			return "", "", fmt.Errorf("librarystore: no GitHub owner configured for python libraries (workspace libraries.publish.python.owner)")
		}
		importPath = fmt.Sprintf("github.com/%s/%s", cfg.PythonOwner, repositoryName(language, name))
		return importPath, fmt.Sprintf("pip install \"git+https://%s@%s\"", importPath, tag), nil
	case LanguageTypeScript:
		if cfg.NpmRegistry == "" || cfg.NpmScope == "" {
			return "", "", fmt.Errorf("librarystore: no npm registry/scope configured for typescript libraries (workspace libraries.publish.typescript)")
		}
		importPath = cfg.NpmScope + "/" + name
		return importPath, fmt.Sprintf("npm install %s@%s", importPath, v.String()), nil
	default:
		return "", "", fmt.Errorf("librarystore: unsupported language %q", language)
	}
}

// Store publishes and resolves library exports through some backend.
type Store interface {
	// Publish uploads the artifact tree at artifactDir as the given coordinates
	// and returns the durable, resolvable location. Publishing a version that
	// already exists is an error: published versions are immutable.
	Publish(ctx context.Context, artifactDir string, c Coordinates) (Published, error)
	// Resolve selects the highest published version satisfying constraint and
	// returns its durable location.
	Resolve(ctx context.Context, language Language, name, constraint string) (Published, error)
	// List returns the published semantic versions for a library export, without
	// a leading "v", newest first.
	List(ctx context.Context, language Language, name string) ([]string, error)
}
