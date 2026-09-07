# Internal Library System Design

## Problem Statement

Codefly services often share internal code (utilities, domain models, clients). Currently there's no way to:
- Share code between services within a workspace
- Version internal shared code independently
- Track which services depend on which internal libraries
- Manage breaking changes in shared code

**This is NOT about external packages** (npm, go mod, pip) - those are handled by language toolchains. This is about internal shared code that:
- Lives within the codefly workspace (or linked via git submodule)
- Needs independent versioning
- Is consumed by multiple services
- May need to be published/distributed

## Design Goals

1. **Git-native versioning** - Use git tags/branches for versions
2. **Submodule support** - Libraries can be external repos linked as submodules
3. **Language-aware** - Different agents for Go, Python, TypeScript, etc.
4. **Dependency tracking** - Know which services use which library versions
5. **Breaking change detection** - Warn when library changes affect consumers
6. **Simple workflow** - `codefly add library`, `codefly update library`

## Architecture

### Library Hierarchy

```
workspace/
├── workspace.codefly.yaml
├── libraries/                    # Workspace-level libraries
│   ├── shared-models/
│   │   ├── library.codefly.yaml
│   │   ├── .git/                 # Can be submodule
│   │   └── go/                   # Language-specific code
│   │       ├── models.go
│   │       └── go.mod
│   └── utils/
│       ├── library.codefly.yaml
│       └── python/
│           └── utils/
│               └── __init__.py
└── modules/
    └── backend/
        └── services/
            └── api/
                ├── service.codefly.yaml  # References libraries
                └── main.go
```

### Configuration Files

**library.codefly.yaml**
```yaml
kind: library
name: shared-models
description: "Shared domain models for the platform"
version: 1.2.0

# Language-specific builds
languages:
  - name: go
    agent: codefly.ai/library-go:0.1.0
    path: go/
    exports:
      - package: github.com/myorg/shared-models
  - name: python
    agent: codefly.ai/library-python:0.1.0
    path: python/
    exports:
      - package: shared_models

# Git configuration (for submodule or standalone)
git:
  remote: git@github.com:myorg/shared-models.git  # Optional
  branch: main

# Dependencies on other libraries
library-dependencies:
  - name: utils
    version: ">=1.0.0"
```

**service.codefly.yaml (updated)**
```yaml
kind: service
name: api
module: backend
version: 0.0.1
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.6
  publisher: codefly.ai

# NEW: Library dependencies
library-dependencies:
  - name: shared-models
    version: ">=1.0.0, <2.0.0"  # Semver constraints
    languages:
      - go  # Only need Go exports for this service

service-dependencies:
  - name: store
    module: backend
```

### Proto Definitions

**proto/codefly/base/v0/library.proto**
```protobuf
syntax = "proto3";

package codefly.base.v0;

option go_package = "github.com/codefly-dev/core/generated/go/codefly/base/v0";

message Library {
    string name = 1;
    string description = 2;
    string version = 3;
    repeated LanguageExport languages = 4;
    GitConfig git = 5;
    repeated LibraryReference library_dependencies = 6;
}

message LanguageExport {
    string name = 1;          // "go", "python", "typescript"
    string agent = 2;         // Agent identifier
    string path = 3;          // Relative path to language code
    repeated string exports = 4;  // Package/module names exported
}

message GitConfig {
    string remote = 1;        // Git remote URL
    string branch = 2;        // Default branch
    string commit = 3;        // Pinned commit (optional)
}

message LibraryReference {
    string name = 1;
    string version = 2;       // Semver constraint
}

message LibraryIdentity {
    string name = 1;
    string workspace = 2;
    string version = 3;
    string path = 4;
}
```

**proto/codefly/actions/v0/library.proto**
```protobuf
syntax = "proto3";

package codefly.actions.v0;

option go_package = "github.com/codefly-dev/core/generated/go/codefly/actions/v0";

import "codefly/base/v0/library.proto";
import "codefly/base/v0/agent.proto";

message AddLibrary {
    string kind = 1;
    string name = 2;
    string description = 3;
    repeated string languages = 4;  // ["go", "python"]
    base.v0.GitConfig git = 5;
}

message UpdateLibrary {
    string kind = 1;
    string name = 2;
    string version = 3;  // New version
}

message AddLibraryDependency {
    string kind = 1;
    string service_name = 2;
    string service_module = 3;
    string library_name = 4;
    string version_constraint = 5;
    repeated string languages = 6;
}
```

## Core Implementation

### resources/library.go

```go
package resources

type Library struct {
    Kind        string              `yaml:"kind"`
    Name        string              `yaml:"name"`
    Description string              `yaml:"description,omitempty"`
    Version     string              `yaml:"version"`
    Languages   []*LanguageExport   `yaml:"languages,omitempty"`
    Git         *GitConfig          `yaml:"git,omitempty"`
    LibraryDeps []*LibraryReference `yaml:"library-dependencies,omitempty"`

    // Internal
    dir string
}

type LanguageExport struct {
    Name    string   `yaml:"name"`    // "go", "python"
    Agent   string   `yaml:"agent"`   // Agent identifier
    Path    string   `yaml:"path"`    // Relative path
    Exports []string `yaml:"exports"` // Package names
}

type GitConfig struct {
    Remote string `yaml:"remote,omitempty"`
    Branch string `yaml:"branch,omitempty"`
    Commit string `yaml:"commit,omitempty"`
}

type LibraryReference struct {
    Name    string `yaml:"name"`
    Version string `yaml:"version"` // Semver constraint
}

// For services
type LibraryDependency struct {
    Name      string   `yaml:"name"`
    Version   string   `yaml:"version"`   // Semver constraint
    Languages []string `yaml:"languages"` // Which language exports needed
}
```

### Version Resolution

```go
package resources

import "github.com/Masterminds/semver/v3"

type LibraryResolver struct {
    workspace *Workspace
    cache     map[string]*Library
}

// ResolveVersion finds the best matching version for a constraint
func (r *LibraryResolver) ResolveVersion(name, constraint string) (*Library, error) {
    lib, err := r.workspace.LoadLibrary(name)
    if err != nil {
        return nil, err
    }

    c, err := semver.NewConstraint(constraint)
    if err != nil {
        return nil, err
    }

    v, err := semver.NewVersion(lib.Version)
    if err != nil {
        return nil, err
    }

    if !c.Check(v) {
        return nil, fmt.Errorf("library %s version %s does not satisfy %s",
            name, lib.Version, constraint)
    }

    return lib, nil
}

// GetAvailableVersions returns all git tags as versions
func (r *LibraryResolver) GetAvailableVersions(lib *Library) ([]*semver.Version, error) {
    if lib.Git == nil || lib.Git.Remote == "" {
        // Local library - only current version
        v, _ := semver.NewVersion(lib.Version)
        return []*semver.Version{v}, nil
    }

    // Get tags from git remote
    return r.getGitTags(lib.Git.Remote)
}
```

### Git Submodule Integration

```go
package resources

import (
    "os/exec"
    "path/filepath"
)

// AddLibraryAsSubmodule adds an external library as git submodule
func (w *Workspace) AddLibraryAsSubmodule(ctx context.Context, name, remote, branch string) (*Library, error) {
    libDir := filepath.Join(w.Dir(), "libraries", name)

    // Add as submodule
    cmd := exec.CommandContext(ctx, "git", "submodule", "add",
        "-b", branch, remote, libDir)
    cmd.Dir = w.Dir()
    if err := cmd.Run(); err != nil {
        return nil, err
    }

    // Load the library config from submodule
    return LoadLibraryFromDir(ctx, libDir)
}

// UpdateLibrarySubmodule updates a library submodule to a specific version
func (w *Workspace) UpdateLibrarySubmodule(ctx context.Context, name, version string) error {
    libDir := filepath.Join(w.Dir(), "libraries", name)

    // Checkout the version tag
    cmd := exec.CommandContext(ctx, "git", "checkout", "v"+version)
    cmd.Dir = libDir
    return cmd.Run()
}

// SyncLibrarySubmodules ensures all submodules are at their declared versions
func (w *Workspace) SyncLibrarySubmodules(ctx context.Context) error {
    cmd := exec.CommandContext(ctx, "git", "submodule", "update", "--init", "--recursive")
    cmd.Dir = w.Dir()
    return cmd.Run()
}
```

## CLI Commands

### codefly add library

```bash
# Create a new local library
codefly add library shared-models --languages=go,python

# Add an external library as submodule
codefly add library shared-models \
  --git=git@github.com:myorg/shared-models.git \
  --branch=main
```

### codefly add library-dependency

```bash
# Add library dependency to a service
codefly add library-dependency shared-models \
  --service=api \
  --module=backend \
  --version=">=1.0.0"
```

### codefly update library

```bash
# Bump library version
codefly update library shared-models --version=1.3.0

# Update submodule to latest tag
codefly update library shared-models --latest

# Check for breaking changes
codefly update library shared-models --check
```

### codefly list libraries

```bash
# List all libraries
codefly list libraries

# Show library details
codefly list libraries --verbose

# Show which services use a library
codefly list libraries --consumers
```

## Library Agents

Library agents handle language-specific tasks:

### go-library agent

```yaml
kind: library-agent
name: go-library
publisher: codefly.ai
version: 0.1.0

capabilities:
  - build
  - test
  - publish

runtime-requirements:
  - go >= 1.21

commands:
  build: go build ./...
  test: go test ./...
  publish: |
    go mod tidy
    git tag v{{version}}
```

### python-library agent

```yaml
kind: library-agent
name: python-library
publisher: codefly.ai
version: 0.1.0

capabilities:
  - build
  - test
  - publish

runtime-requirements:
  - python >= 3.10
  - poetry

commands:
  build: poetry build
  test: poetry run pytest
  publish: poetry publish
```

## Workflow Example

### Creating a shared library

```bash
# 1. Create the library
codefly add library shared-models --languages=go

# 2. Add code to libraries/shared-models/go/
# ...write your models...

# 3. Tag a version
codefly update library shared-models --version=1.0.0
```

### Using the library in a service

```bash
# 1. Add dependency
codefly add library-dependency shared-models \
  --service=api --module=backend --version="^1.0.0"

# 2. Service agent automatically:
#    - Adds go.mod replace directive (for local dev)
#    - Updates imports
#    - Configures build to include library
```

### Handling breaking changes

```bash
# 1. Update library with breaking change
codefly update library shared-models --version=2.0.0

# 2. Check impact
codefly update library shared-models --check
# Output:
#   WARNING: Breaking change detected
#   Affected services:
#     - backend/api (uses ^1.0.0)
#     - backend/worker (uses ^1.0.0)
#   Run 'codefly update library-dependency' to update consumers

# 3. Update consumers
codefly update library-dependency shared-models \
  --service=api --module=backend --version="^2.0.0"
```

## File Structure

```
core/
├── resources/
│   ├── library.go           # Library type and methods
│   ├── library_reference.go # LibraryReference, LibraryDependency
│   └── library_resolver.go  # Version resolution
├── agents/
│   └── library/
│       ├── agent.go         # Library agent interface
│       └── helpers/         # Language-specific helpers

cli/
├── cmd/
│   ├── add/
│   │   ├── library.go              # codefly add library
│   │   └── library_dependency.go   # codefly add library-dependency
│   ├── update/
│   │   └── library.go              # codefly update library
│   └── list/
│       └── libraries.go            # codefly list libraries

agents/
└── libraries/
    ├── go-library/
    │   ├── agent.codefly.yaml
    │   └── templates/factory/
    └── python-library/
        ├── agent.codefly.yaml
        └── templates/factory/

proto/
└── codefly/
    ├── base/v0/
    │   └── library.proto
    └── actions/v0/
        └── library.proto
```

## Generated client libraries

`codefly generate client` (see `docs/commands.md`) is the library system's first
real producer: it turns an API contract (a local service, a composed module
package, or a `generate contracts` export) into a library under
`libraries/<name>/<language>/` — generated bindings plus a generated
gateway-bound facade — with the same `libraries/<name>/library.codefly.yaml`
this design describes, extended with a `sources:` list (always a list, even
for `generate client`'s single contract, so it shares one schema with
`sync solution-sdk`'s aggregated libraries below):

```yaml
sources:
  - package: codefly/saas-starter      # or "local" for a live service
    version: 0.1.0
    module: saas-starter
    service: accounts
    endpoint: connect
    contract-digest: sha256:…
generated-by:
  codefly: 0.1.140
  companion: codeflydev/proto:0.0.13
  facade: true
```

`sources[].contract-digest` is the drift signal: it is the digest of the
contract (protobuf descriptor set or OpenAPI document) the library was
generated from. Re-running `generate client` against an unchanged contract
regenerates in place (idempotent, byte-identical output); against a changed
one, it refuses to overwrite the library unless `--force` is given, so a stale
generated library is always an explicit choice, never a silent one.

This complements, rather than replaces, the git-submodule and hand-authored
paths above: a generated library still lives at `libraries/<name>/`, is loaded
by `resources.Library` like any other (the `sources:`/`generated-by:` fields
are additive — a plain YAML decode ignores fields it does not declare), and
flows through `add library-dependency`, `sync library-dependencies`, and
`publish library` unchanged.

### Solution SDKs

A solution declares what it consumes once, in `api.consumes` on
`solution.codefly.yaml` (core's `solution/manifest` package) — module, service,
endpoint, an optional version constraint, an optional services subset, and an
optional facade entry-point override (`as`). `codefly sync solution-sdk`
aggregates every module-bound entry into **one** library per solution,
`libraries/<solution module>-sdk/`, sharing this same schema and the same
generator `pkg/generators.GenerateClientLibrary` as `generate client` —
`generate client` calls it with one contract entry, `sync solution-sdk` with
one per bound `api.consumes` entry, all targeting the same output root so
every language shares one `go.mod`/`package.json`/`pyproject.toml`:

```yaml
sources:
  - package: codefly/saas-starter
    version: 0.1.0
    module: saas-starter
    service: accounts
    endpoint: connect
    contract-digest: sha256:…
    services: [AuditService]
  - package: codefly/other-module
    version: 0.2.1
    module: other-module
    service: billing
    endpoint: connect
    contract-digest: sha256:…
```

Two entries whose contracts share a protobuf package at different digests are
rejected as a diamond, the same rule `pkg/composition/coherence.go` enforces
for a service's transitive library closure, applied here to the contracts one
generation aggregates.

This single declaration drives three outputs, kept in step by the same
command: the generated SDK above; the entry service's runtime
`service-dependencies` (`--apply-dependencies` adds any entry missing there,
never removing an existing one); and the `SolutionBundle`'s
`platform.consumes`, which lodestar derives from the same manifest. Before
`api.consumes`, a solution declared what it consumed in up to four
disconnected places — `service-dependencies`, a vendored `solution-sdk.yaml`,
a `go.mod` replace pin, and a hand-written `Manifest{}` struct for host
registration — that could silently drift apart; `sync solution-sdk` replaces
the vendoring path (`solution-sdk.yaml` + a fetched `_sdk/` directory) with a
fail-closed one: a composed module that ships no API contract catalog
(`generate contracts` not yet run) is an error, not a stale fetch.

## Migration Path

1. **Phase 1**: Proto definitions + core types
2. **Phase 2**: CLI commands (add, list)
3. **Phase 3**: Service integration (library-dependencies)
4. **Phase 4**: Library agents (go-library, python-library)
5. **Phase 5**: Git submodule automation
6. **Phase 6**: Breaking change detection

## Open Questions

1. **Where do libraries live?**
   - Workspace-level (`workspace/libraries/`) - current design
   - Module-level (`module/libraries/`) - alternative
   - Both? (module-specific vs shared)

2. **Version source of truth**
   - Git tags as primary source
   - library.codefly.yaml version field synced from tags?

3. **Publishing/Distribution** — answered: `codefly publish library` (`pkg/librarystore`)
   publishes each language export through its native package manager rather than a
   codefly-specific registry:
   - **Go**: a GitHub repository (`github.com/<owner>/<name>-go`) tagged at an
     immutable semver version — resolvable by `go get`/GOPROXY with no codefly
     toolchain.
   - **TypeScript**: an npm-compatible registry (GitHub Packages under
     `codefly-dev`, or a workspace-configured BYO feed) — resolvable by `npm install`.
   - **Python**: a GitHub repository (`github.com/<owner>/<name>-python`) tagged the
     same way as Go — resolvable by `pip install git+...@vX.Y.Z`. A PyPI-style feed
     is a later decision.

   Git submodules remain the local-linking mechanism (`codefly add library --git`);
   publishing is the separate, opt-in path for sharing a library outside the
   workspace it was authored in.

4. **Cross-workspace sharing**
   - How to share libraries between workspaces?
   - Git submodules solve this naturally
