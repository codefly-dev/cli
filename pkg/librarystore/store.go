// Package librarystore publishes and resolves codefly libraries as durable,
// versioned artifacts consumable by a language's native package manager
// (go get / pip / npm), so a consumer needs neither the codefly toolchain nor
// local source. The Store interface is backend-agnostic; the GitHub-backed
// implementation is the first (and, for now, only) backend.
package librarystore

import "context"

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
	// Digest is the sha256 over the published artifact tree ("sha256:<hex>"),
	// set by Publish from the source tree and by Resolve from the published
	// content at the resolved version. The two agree for a store-published
	// version.
	Digest string
	// InstallHint is a copy-pasteable native install command.
	InstallHint string
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
