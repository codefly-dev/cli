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
	// Warnings are operator-facing notes about the publish that are not
	// failures: the publish succeeded and the version is live. A store reports
	// them as data rather than printing them so the command owns presentation
	// and a test can assert them without capturing output. The publish commands
	// print each one after the result table.
	Warnings []string
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

// RepositoryPolicy is the caller's decision about bringing an export's
// repository into existence. It is a separate parameter rather than a
// StoreConfig field because StoreConfig is exactly what LoadStoreConfig parses
// out of workspace.codefly.yaml: a policy expressible in a file that travels
// with a module is a policy a module can grant itself. Keeping it out of that
// struct makes "the workspace authorized its own repository" unrepresentable
// instead of merely commented against, and leaves no field for a future
// extension of the YAML schema to reach.
//
// The zero value creates nothing, which is what every read path (install, list)
// and every publish without an explicit flag passes.
type RepositoryPolicy struct {
	// CreateMissing lets a publish create the repository when it is absent.
	CreateMissing bool
	// Public creates it public rather than private. It is meaningful only
	// together with CreateMissing — Validate rejects it alone rather than
	// letting a typed flag be silently discarded.
	Public bool
}

// Validate rejects a policy whose visibility choice could not take effect.
// --public-repository without --create-missing-repository used to be accepted
// and then dropped on the floor: nothing creates a repository, so nothing reads
// the visibility, and the operator was left believing they had set one. An
// unusable flag combination is a usage error, not a default.
func (p RepositoryPolicy) Validate() error {
	if p.Public && !p.CreateMissing {
		return fmt.Errorf(
			"librarystore: --public-repository requires --create-missing-repository (visibility only applies to a repository this publish creates; an existing repository's visibility is not changed)")
	}
	return nil
}

// visibility reports the visibility a repository created under this policy gets.
// It is meaningful only when CreateMissing is set.
func (p RepositoryPolicy) visibility() repositoryVisibility {
	if p.Public {
		return visibilityPublic
	}
	return visibilityPrivate
}

// repositoryVisibility is what a store knows about the published repository's
// visibility. Unknown is the honest answer whenever no API call was made — a
// publish that creates nothing never looks the repository up, and the no-network
// preview cannot look anything up by contract.
type repositoryVisibility int

const (
	visibilityUnknown repositoryVisibility = iota
	visibilityPublic
	visibilityPrivate
)

// newConfiguredGitHubStore applies the caller's repository-creation policy. A
// store built from the zero policy never creates anything.
func newConfiguredGitHubStore(owner string, policy RepositoryPolicy) *GitHubStore {
	store := NewGitHubStore(owner)
	if policy.CreateMissing {
		store.EnableRepositoryCreation(policy)
	}
	return store
}

// NewStoreFor returns the Store backend for language, configured from cfg and
// policy. It rejects an unusable policy before building anything, so every
// caller inherits the check rather than each command repeating it.
func NewStoreFor(language Language, cfg StoreConfig, policy RepositoryPolicy) (Store, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	switch language {
	case LanguageGo:
		if cfg.GoOwner == "" {
			return nil, fmt.Errorf("librarystore: no GitHub owner configured for go libraries (workspace libraries.publish.go.owner)")
		}
		return newConfiguredGitHubStore(cfg.GoOwner, policy), nil
	case LanguagePython:
		if cfg.PythonOwner == "" {
			return nil, fmt.Errorf("librarystore: no GitHub owner configured for python libraries (workspace libraries.publish.python.owner)")
		}
		return newConfiguredGitHubStore(cfg.PythonOwner, policy), nil
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
func PreviewIdentity(language Language, cfg StoreConfig, policy RepositoryPolicy, name, version string) (importPath, installHint string, err error) {
	if err = policy.Validate(); err != nil {
		return "", "", err
	}
	v, err := semver.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return "", "", fmt.Errorf("librarystore: %q is not a semantic version: %w", version, err)
	}
	// A preview makes no network call, so it can only know the visibility it is
	// about to impose: when the policy creates the repository, that is exactly
	// what the publish will do. Without creation the repository already exists
	// and only GitHub knows how it is configured, so the hint stays in its
	// public form — the documented contract of a published export — rather than
	// guessing.
	visibility := visibilityUnknown
	if policy.CreateMissing {
		visibility = policy.visibility()
	}
	switch language {
	case LanguageGo:
		if cfg.GoOwner == "" {
			return "", "", fmt.Errorf("librarystore: no GitHub owner configured for go libraries (workspace libraries.publish.go.owner)")
		}
		importPath = fmt.Sprintf("github.com/%s/%s", cfg.GoOwner, repositoryName(language, name))
		return importPath, goInstallHint(importPath, v.String(), visibility), nil
	case LanguagePython:
		if cfg.PythonOwner == "" {
			return "", "", fmt.Errorf("librarystore: no GitHub owner configured for python libraries (workspace libraries.publish.python.owner)")
		}
		importPath = fmt.Sprintf("github.com/%s/%s", cfg.PythonOwner, repositoryName(language, name))
		return importPath, pythonInstallHint(importPath, v.String(), visibility), nil
	case LanguageTypeScript:
		if cfg.NpmRegistry == "" || cfg.NpmScope == "" {
			return "", "", fmt.Errorf("librarystore: no npm registry/scope configured for typescript libraries (workspace libraries.publish.typescript)")
		}
		importPath = cfg.NpmScope + "/" + name
		return importPath, npmInstallHint(importPath, v.String()), nil
	default:
		return "", "", fmt.Errorf("librarystore: unsupported language %q", language)
	}
}

// goInstallHint, pythonInstallHint, and npmInstallHint are each backend's
// single source of truth for its InstallHint format string. Both
// PreviewIdentity (the --dry-run / pre-flight preview, which never touches
// the network) and the real Published a store's Publish/Resolve returns call
// through these, so the two can never drift apart the way two independently
// maintained copies of the same format string could.
// A private repository changes what the Go command has to be: `go get` alone
// routes through the public module proxy, which cannot see a private
// repository and fails, so the hint must carry GOPRIVATE for the owner's
// namespace. Emitting the bare public form for a repository this tool just
// created private hands the operator a command that cannot work.
//
// pip needs no such flag: `git+https://` delegates to git, which uses the same
// credential helper for a private repository as for a public one, so the
// command is identical and the visibility argument correctly changes nothing.
func goInstallHint(importPath, version string, visibility repositoryVisibility) string {
	get := fmt.Sprintf("go get %s@%s", importPath, versionTag(version))
	if visibility != visibilityPrivate {
		return get
	}
	return fmt.Sprintf("GOPRIVATE=%s go get %s@%s", goPrivatePattern(importPath), importPath, versionTag(version))
}

// goPrivatePattern is the GOPRIVATE glob covering the owner an export publishes
// under — "github.com/<owner>/*" — so one setting serves every library that
// owner publishes rather than one entry per repository.
func goPrivatePattern(importPath string) string {
	if owner, _, found := strings.Cut(strings.TrimPrefix(importPath, "github.com/"), "/"); found {
		return "github.com/" + owner + "/*"
	}
	return importPath
}

func pythonInstallHint(importPath, version string, _ repositoryVisibility) string {
	return fmt.Sprintf("pip install %q", PipGitArgument(importPath, version))
}

// PipGitArgument returns the pip install argument for a GitHub-backed Python
// export: git+https://<importPath>@v<version>. Exposed so a caller invoking
// pip directly (codefly install library --destination) builds the exact
// argument pythonInstallHint's InstallHint documents from Published.ImportPath,
// instead of re-deriving a git URL from Published.Location and risking the
// two diverge.
func PipGitArgument(importPath, version string) string {
	return fmt.Sprintf("git+https://%s@%s", importPath, versionTag(version))
}

func npmInstallHint(importPath, version string) string {
	return fmt.Sprintf("npm install %s@%s", importPath, version)
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
