# Library Build & Mount Strategy

## The Challenge

When a service depends on a library:
1. **Local dev (native)**: Library code needs to be accessible to the service build
2. **Docker build**: Library needs to be mounted/copied into the container
3. **Version conflicts**: Different services may need different library versions
4. **Hot reload**: Changes to library should propagate to services

## Language-Specific Challenges

### Go Libraries

**Problem**: Go uses `go.mod` with module paths. A shared library needs:
- A module path (e.g., `github.com/myorg/shared-models`)
- Version tags for `go get`
- `replace` directives for local development

**Solution**: Use `replace` directives in development, real versions in production

```go
// service/go.mod (development)
module github.com/myorg/backend-api

require github.com/myorg/shared-models v1.2.0

// Auto-managed by codefly
replace github.com/myorg/shared-models => ../../libraries/shared-models/go
```

```go
// service/go.mod (production build)
module github.com/myorg/backend-api

require github.com/myorg/shared-models v1.2.0
// No replace - uses real published version or vendor
```

### Python Libraries

**Problem**: Python uses `pip` and `poetry` with package names

**Solution**: Use editable installs (`pip install -e`) in development

```toml
# service/pyproject.toml
[tool.poetry.dependencies]
shared-models = { path = "../../libraries/shared-models/python", develop = true }

# OR for production
shared-models = { git = "https://github.com/myorg/shared-models.git", tag = "v1.2.0", subdirectory = "python" }
```

### TypeScript/Node Libraries

**Problem**: npm/yarn with package.json

**Solution**: Use `npm link` or workspace references

```json
// service/package.json
{
  "dependencies": {
    "@myorg/shared-models": "file:../../libraries/shared-models/typescript"
  }
}
```

## Build Strategies

### Strategy 1: Replace/Link at Build Time (Recommended)

The service agent manages `go.mod`/`pyproject.toml`/`package.json` to:
1. Add `replace`/`link` directives pointing to local library paths
2. Before production build, remove replaces and use real versions

**Pros**:
- Simple, uses native tooling
- Hot reload works naturally
- No copying needed

**Cons**:
- Service config files modified by codefly
- Need to manage replace directives carefully

### Strategy 2: Vendor/Copy at Build Time

Copy library source into service before building:

```
service/
├── vendor/
│   └── shared-models/   # Copied from libraries/shared-models
└── main.go
```

**Pros**:
- Clean isolation
- Works with any tooling

**Cons**:
- Slower (copy on every build)
- Hot reload harder
- Disk space duplication

### Strategy 3: Mount in Docker

For Docker builds, mount library as a volume:

```yaml
# docker-compose.yml (generated)
services:
  api:
    build:
      context: .
    volumes:
      - ../../libraries/shared-models:/libraries/shared-models:ro
```

Then in Dockerfile:
```dockerfile
# Copy from mounted location
COPY --from=libraries /libraries/shared-models/go /app/vendor/shared-models
```

**Pros**:
- No duplication
- Works well with Docker cache

**Cons**:
- More complex Docker setup
- Need to coordinate paths

## Recommended Approach

### For Development (Native)

1. **Go**: Auto-manage `replace` directives in `go.mod`
2. **Python**: Use editable installs via poetry
3. **TypeScript**: Use workspace links

```go
// core/resources/library_resolver.go

// SetupLocalDevelopment configures a service for local library development
func (r *LibraryResolver) SetupLocalDevelopment(ctx context.Context, svc *Service) error {
    for _, dep := range svc.LibraryDependencies {
        lib, err := r.workspace.LoadLibraryFromName(ctx, dep.Name)
        if err != nil {
            return err
        }

        for _, lang := range dep.Languages {
            switch lang {
            case "go":
                err = r.setupGoReplace(ctx, svc, lib)
            case "python":
                err = r.setupPythonEditable(ctx, svc, lib)
            case "typescript":
                err = r.setupNpmLink(ctx, svc, lib)
            }
            if err != nil {
                return err
            }
        }
    }
    return nil
}

func (r *LibraryResolver) setupGoReplace(ctx context.Context, svc *Service, lib *Library) error {
    goLang := lib.GetLanguage("go")
    if goLang == nil {
        return fmt.Errorf("library %s has no go export", lib.Name)
    }

    // Calculate relative path from service to library
    relPath, err := filepath.Rel(svc.Dir(), lib.LanguagePath(goLang))
    if err != nil {
        return err
    }

    // Get the Go module path from library exports
    if len(goLang.Exports) == 0 {
        return fmt.Errorf("library %s go export has no module path", lib.Name)
    }
    modulePath := goLang.Exports[0]

    // Add replace directive to service's go.mod
    return r.addGoReplace(ctx, svc.Dir(), modulePath, relPath)
}

func (r *LibraryResolver) addGoReplace(ctx context.Context, svcDir, modulePath, relPath string) error {
    goModPath := filepath.Join(svcDir, "go.mod")

    // Use go mod edit to add replace
    cmd := exec.CommandContext(ctx, "go", "mod", "edit",
        "-replace", fmt.Sprintf("%s=%s", modulePath, relPath))
    cmd.Dir = svcDir
    return cmd.Run()
}
```

### For Docker Build

1. **Multi-stage build with library mount**:

```dockerfile
# Stage 1: Build with libraries mounted
FROM golang:1.22 AS builder

# Copy service code
COPY service/ /app/

# Libraries are mounted at build time (via --mount)
RUN --mount=type=bind,source=libraries/shared-models/go,target=/libraries/shared-models \
    cd /app && \
    go mod edit -replace github.com/myorg/shared-models=/libraries/shared-models && \
    go build -o /app/main .

# Stage 2: Runtime
FROM alpine:latest
COPY --from=builder /app/main /app/main
CMD ["/app/main"]
```

2. **Docker Compose for development**:

```yaml
services:
  api:
    build:
      context: ../..
      dockerfile: modules/backend/services/api/Dockerfile
    volumes:
      # Mount libraries for hot reload
      - ../../libraries:/libraries:ro
```

### For Production Build (CI/CD)

Option A: **Use git tags as real versions**
- Libraries are published to git with version tags
- Services use real `require` statements without `replace`
- CI/CD pulls specific library versions

Option B: **Vendor at build time**
- Build script copies libraries to vendor before build
- Clean, reproducible builds
- Larger repo/artifacts

```bash
# ci/build.sh
#!/bin/bash
# Copy libraries to vendor
for lib in $(codefly list libraries --names); do
    version=$(codefly library-version $lib --constraint "$(codefly service-library-constraint api $lib)")
    cp -r libraries/$lib/go vendor/$lib
done

# Build with vendored libraries
cd services/api
go build -mod=vendor
```

## Version Resolution

### Constraint Format

Use semver constraints (compatible with npm/cargo/poetry):
- `^1.2.0` - Compatible with 1.2.0 (>=1.2.0 <2.0.0)
- `~1.2.0` - Approximately 1.2.0 (>=1.2.0 <1.3.0)
- `>=1.0.0,<2.0.0` - Range
- `1.2.0` - Exact version

### Resolution Algorithm

```go
func (r *LibraryResolver) Resolve(ctx context.Context, name, constraint string) (*Library, string, error) {
    lib, err := r.workspace.LoadLibraryFromName(ctx, name)
    if err != nil {
        return nil, "", err
    }

    // Get available versions
    versions, err := lib.GetAvailableVersions(ctx)
    if err != nil {
        return nil, "", err
    }

    // Parse constraint
    c, err := semver.NewConstraint(constraint)
    if err != nil {
        return nil, "", err
    }

    // Find best matching version
    var best *semver.Version
    for _, v := range versions {
        if c.Check(v) {
            if best == nil || v.GreaterThan(best) {
                best = v
            }
        }
    }

    if best == nil {
        return nil, "", fmt.Errorf("no version of %s satisfies %s", name, constraint)
    }

    return lib, best.String(), nil
}
```

## Implementation Plan

### Phase 1: Go Support
1. `LibraryResolver` with `setupGoReplace`
2. Auto-manage `go.mod` replace directives
3. Test with real service + library

### Phase 2: Docker Integration
1. Update `DockerEnvironment` to mount libraries
2. Generate Dockerfile with library handling
3. Test Docker builds with libraries

### Phase 3: Python/TypeScript
1. Add Python editable install support
2. Add npm link support
3. Test cross-language scenarios

### Phase 4: CI/CD
1. `codefly build` handles library resolution
2. `codefly ci build` with vendoring option
3. Document production build patterns

## Testing Strategy

```go
// library_resolver_test.go

func TestLibraryResolver_GoReplace(t *testing.T) {
    // 1. Create test workspace with library and service
    // 2. Call SetupLocalDevelopment
    // 3. Verify go.mod has replace directive
    // 4. Build service and verify it works
}

func TestLibraryResolver_VersionResolution(t *testing.T) {
    // 1. Create library with multiple git tags
    // 2. Test various constraints
    // 3. Verify correct version selected
}

func TestLibraryResolver_DockerBuild(t *testing.T) {
    // 1. Create service with library dependency
    // 2. Generate Dockerfile
    // 3. Build Docker image
    // 4. Verify library code included correctly
}
```
