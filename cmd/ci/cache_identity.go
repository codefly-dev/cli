package ci

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
)

const (
	cacheIdentitySchemaVersion = 1
	cacheIdentityAlgorithm     = "sha256"
	cacheStatusIdentityOnly    = "identity_only"
	cacheStatusUnavailable     = "unavailable"
)

// CICacheIdentity is a content-addressed description of a CI task. Version 1
// only reports identities; a later cache store may turn identity_only into a
// hit/miss outcome without changing the task or key contract.
type CICacheIdentity struct {
	SchemaVersion int                  `json:"schema_version"`
	Algorithm     string               `json:"algorithm"`
	Key           string               `json:"key,omitempty"`
	Status        string               `json:"status"`
	Inputs        CICacheIdentityInput `json:"inputs"`
	Limitations   []string             `json:"limitations,omitempty"`
}

type CICacheIdentityInput struct {
	CodeflyVersion  string                  `json:"codefly_version"`
	CoreVersion     string                  `json:"core_version"`
	Platform        string                  `json:"platform"`
	RuntimeContext  string                  `json:"runtime_context"`
	Phase           string                  `json:"phase"`
	Suite           string                  `json:"suite,omitempty"`
	Service         string                  `json:"service"`
	Agent           CICacheAgentInput       `json:"agent"`
	WorkspaceDigest string                  `json:"workspace_digest"`
	ModuleDigest    string                  `json:"module_digest"`
	ServiceDigest   string                  `json:"service_digest"`
	Dependencies    []CICacheResourceDigest `json:"dependencies"`
	Libraries       []CICacheResourceDigest `json:"libraries"`
}

type CICacheAgentInput struct {
	Kind      string `json:"kind"`
	Publisher string `json:"publisher"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Digest    string `json:"digest,omitempty"`
}

type CICacheResourceDigest struct {
	Resource string `json:"resource"`
	Digest   string `json:"digest"`
}

type ciCacheIdentityBuilder struct {
	workspace      *resources.Workspace
	codeflyVersion string
	repoRoot       string
	gitFiles       []string
	useGitFiles    bool
	digestCache    map[string]string
}

func newCICacheIdentityBuilder(ctx context.Context, workspace *resources.Workspace, codeflyVersion string) *ciCacheIdentityBuilder {
	builder := &ciCacheIdentityBuilder{
		workspace:      workspace,
		codeflyVersion: strings.TrimSpace(codeflyVersion),
		digestCache:    map[string]string{},
	}
	if workspace == nil {
		return builder
	}
	repoRoot, err := gitRoot(ctx, workspace.Dir())
	if err != nil {
		return builder
	}
	files, err := gitCacheFiles(ctx, repoRoot)
	if err != nil {
		return builder
	}
	builder.repoRoot = cleanAbs(repoRoot)
	builder.gitFiles = files
	builder.useGitFiles = true
	return builder
}

func gitCacheFiles(ctx context.Context, repoRoot string) ([]string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repoRoot, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	payload, err := command.Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, raw := range strings.Split(string(payload), "\x00") {
		if raw == "" {
			continue
		}
		path := cleanAbs(filepath.Join(repoRoot, filepath.FromSlash(raw)))
		if seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	sort.Strings(files)
	return files, nil
}

func (builder *ciCacheIdentityBuilder) identity(ctx context.Context, options ScheduleOptions, planned PlannedService) CICacheIdentity {
	identity := CICacheIdentity{
		SchemaVersion: cacheIdentitySchemaVersion,
		Algorithm:     cacheIdentityAlgorithm,
		Status:        cacheStatusIdentityOnly,
		Inputs: CICacheIdentityInput{
			CodeflyVersion: builder.codeflyVersion,
			CoreVersion:    resources.CLI.Version,
			Platform:       runtime.GOOS + "/" + runtime.GOARCH,
			RuntimeContext: normalizedCacheRuntimeContext(options.RuntimeContext),
			Phase:          strings.TrimSpace(options.Phase),
			Suite:          normalizedCacheSuite(options.Phase, options.Suite),
			Service:        planned.Service,
			Dependencies:   []CICacheResourceDigest{},
			Libraries:      []CICacheResourceDigest{},
		},
	}
	inputs, limitations, err := builder.inputs(ctx, identity.Inputs, planned.Service)
	identity.Inputs = inputs
	identity.Limitations = limitations
	if err != nil {
		identity.Status = cacheStatusUnavailable
		identity.Limitations = append(identity.Limitations, err.Error())
		identity.Limitations = sortedUnique(identity.Limitations)
		return identity
	}
	payload, err := json.Marshal(identity.Inputs)
	if err != nil {
		identity.Status = cacheStatusUnavailable
		identity.Limitations = append(identity.Limitations, fmt.Sprintf("encode cache inputs: %v", err))
		return identity
	}
	identity.Key = "sha256:" + resources.Hash(payload)
	identity.Limitations = sortedUnique(identity.Limitations)
	return identity
}

func (builder *ciCacheIdentityBuilder) workspaceIdentity(options ScheduleOptions, workspaceName string) CICacheIdentity {
	identity := CICacheIdentity{
		SchemaVersion: cacheIdentitySchemaVersion,
		Algorithm:     cacheIdentityAlgorithm,
		Status:        cacheStatusIdentityOnly,
		Inputs: CICacheIdentityInput{
			CodeflyVersion: builder.codeflyVersion,
			CoreVersion:    resources.CLI.Version,
			Platform:       runtime.GOOS + "/" + runtime.GOARCH,
			RuntimeContext: normalizedCacheRuntimeContext(options.RuntimeContext),
			Phase:          strings.TrimSpace(options.Phase),
			Suite:          normalizedCacheSuite(options.Phase, options.Suite),
			Service:        "workspace:" + workspaceName,
			Dependencies:   []CICacheResourceDigest{},
			Libraries:      []CICacheResourceDigest{},
		},
	}
	if builder.workspace == nil {
		identity.Status = cacheStatusUnavailable
		identity.Limitations = []string{"cache identity workspace is nil"}
		return identity
	}
	var err error
	identity.Inputs.WorkspaceDigest, err = builder.workspaceDigest()
	if err == nil {
		identity.Inputs.ServiceDigest, err = builder.digestPath(builder.workspace.Dir())
	}
	if err != nil {
		identity.Status = cacheStatusUnavailable
		identity.Limitations = []string{fmt.Sprintf("hash workspace task inputs: %v", err)}
		return identity
	}
	payload, err := json.Marshal(identity.Inputs)
	if err != nil {
		identity.Status = cacheStatusUnavailable
		identity.Limitations = []string{fmt.Sprintf("encode cache inputs: %v", err)}
		return identity
	}
	identity.Key = "sha256:" + resources.Hash(payload)
	return identity
}

func normalizedCacheRuntimeContext(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "free"
	}
	return value
}

func normalizedCacheSuite(phase, suite string) string {
	suite = strings.TrimSpace(suite)
	if strings.TrimSpace(phase) == "test" && suite == "" {
		return "default"
	}
	return suite
}

func (builder *ciCacheIdentityBuilder) inputs(ctx context.Context, inputs CICacheIdentityInput, serviceUnique string) (CICacheIdentityInput, []string, error) {
	if builder.workspace == nil {
		return inputs, nil, fmt.Errorf("cache identity workspace is nil")
	}
	module, service, err := loadCacheService(ctx, builder.workspace, serviceUnique)
	if err != nil {
		return inputs, nil, err
	}
	if service.Agent == nil {
		return inputs, nil, fmt.Errorf("service %s has no agent", serviceUnique)
	}
	inputs.Agent = CICacheAgentInput{
		Kind:      string(service.Agent.Kind),
		Publisher: service.Agent.Publisher,
		Name:      service.Agent.Name,
		Version:   service.Agent.Version,
	}
	var limitations []string
	agentPath, err := service.Agent.Path(ctx)
	if err != nil {
		limitations = append(limitations, "resolved agent binary path is unavailable")
	} else if _, statErr := os.Lstat(agentPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			limitations = append(limitations, "resolved agent binary is not installed")
		} else {
			limitations = append(limitations, "resolved agent binary cannot be inspected")
		}
	} else {
		inputs.Agent.Digest, err = builder.digestPath(agentPath)
		if err != nil {
			limitations = append(limitations, "resolved agent binary cannot be hashed")
		}
	}

	inputs.WorkspaceDigest, err = builder.workspaceDigest()
	if err != nil {
		return inputs, limitations, fmt.Errorf("hash workspace inputs: %w", err)
	}
	inputs.ModuleDigest, err = builder.moduleDigest(module)
	if err != nil {
		return inputs, limitations, fmt.Errorf("hash module %s inputs: %w", module.Name, err)
	}
	inputs.ServiceDigest, err = builder.digestPath(service.Dir())
	if err != nil {
		return inputs, limitations, fmt.Errorf("hash service %s inputs: %w", serviceUnique, err)
	}

	dependencies, err := architecture.NewServiceDependencies(ctx, builder.workspace)
	if err != nil {
		return inputs, limitations, fmt.Errorf("load cache dependency graph: %w", err)
	}
	order, err := dependencies.OrderTo(ctx, serviceUnique)
	if err != nil {
		return inputs, limitations, fmt.Errorf("resolve cache dependencies for %s: %w", serviceUnique, err)
	}
	servicesForLibraries := []*resources.Service{service}
	for _, dependency := range order {
		dependencyModule, dependencyService, err := loadCacheService(ctx, builder.workspace, dependency.Unique)
		if err != nil {
			return inputs, limitations, err
		}
		servicesForLibraries = append(servicesForLibraries, dependencyService)
		serviceDigest, err := builder.digestPath(dependencyService.Dir())
		if err != nil {
			return inputs, limitations, fmt.Errorf("hash dependency %s: %w", dependency.Unique, err)
		}
		moduleDigest, err := builder.moduleDigest(dependencyModule)
		if err != nil {
			return inputs, limitations, fmt.Errorf("hash dependency module %s: %w", dependencyModule.Name, err)
		}
		inputs.Dependencies = append(inputs.Dependencies, CICacheResourceDigest{
			Resource: dependency.Unique,
			Digest:   aggregateCacheDigests([]CICacheResourceDigest{{Resource: "service", Digest: serviceDigest}, {Resource: "module", Digest: moduleDigest}}),
		})
	}
	sortCacheResourceDigests(inputs.Dependencies)

	inputs.Libraries, err = builder.libraryDigests(ctx, servicesForLibraries)
	if err != nil {
		return inputs, limitations, err
	}
	return inputs, limitations, nil
}

func loadCacheService(ctx context.Context, workspace *resources.Workspace, unique string) (*resources.Module, *resources.Service, error) {
	reference, err := resources.ParseServiceWithOptionalModule(unique)
	if err != nil {
		return nil, nil, fmt.Errorf("parse cache service %s: %w", unique, err)
	}
	module, err := workspace.LoadModuleFromName(ctx, reference.Module)
	if err != nil {
		return nil, nil, fmt.Errorf("load cache module %s: %w", reference.Module, err)
	}
	service, err := module.LoadServiceFromName(ctx, reference.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("load cache service %s: %w", unique, err)
	}
	service.WithModule(module.Name)
	return module, service, nil
}

func (builder *ciCacheIdentityBuilder) workspaceDigest() (string, error) {
	root := builder.workspace.Dir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var paths []cacheDigestPath
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && (strings.HasSuffix(name, ".codefly.yaml") || isSharedToolchainInput(name)) {
			paths = append(paths, cacheDigestPath{Label: name, Path: filepath.Join(root, name)})
		}
	}
	for _, name := range []string{"configurations", "environments"} {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); err == nil {
			paths = append(paths, cacheDigestPath{Label: name, Path: path})
		}
	}
	return builder.digestComponents(paths)
}

func isSharedToolchainInput(name string) bool {
	switch name {
	case "go.work", "go.work.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb", "Cargo.lock", "uv.lock", "poetry.lock", "flake.nix", "flake.lock", "devenv.nix", "devenv.yaml", ".tool-versions", ".nvmrc", "mise.toml":
		return true
	default:
		return false
	}
}

func (builder *ciCacheIdentityBuilder) moduleDigest(module *resources.Module) (string, error) {
	root := module.Dir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var paths []cacheDigestPath
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".codefly.yaml") {
			paths = append(paths, cacheDigestPath{Label: name, Path: filepath.Join(root, name)})
		}
	}
	for _, name := range []string{"configurations", "deployment", "environments"} {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); err == nil {
			paths = append(paths, cacheDigestPath{Label: name, Path: path})
		}
	}
	return builder.digestComponents(paths)
}

func (builder *ciCacheIdentityBuilder) libraryDigests(ctx context.Context, services []*resources.Service) ([]CICacheResourceDigest, error) {
	pending := map[string]bool{}
	for _, service := range services {
		for _, dependency := range service.LibraryDependencies {
			if dependency != nil && dependency.Name != "" {
				pending[dependency.Name] = true
			}
		}
	}
	loaded := map[string]*resources.Library{}
	for len(pending) > 0 {
		var names []string
		for name := range pending {
			names = append(names, name)
		}
		sort.Strings(names)
		name := names[0]
		delete(pending, name)
		if _, exists := loaded[name]; exists {
			continue
		}
		library, err := builder.workspace.LoadLibraryFromName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("load cache library %s: %w", name, err)
		}
		loaded[name] = library
		for _, dependency := range library.LibraryDeps {
			if dependency != nil && dependency.Name != "" {
				pending[dependency.Name] = true
			}
		}
	}
	result := make([]CICacheResourceDigest, 0, len(loaded))
	for name, library := range loaded {
		digest, err := builder.digestPath(library.Dir())
		if err != nil {
			return nil, fmt.Errorf("hash library %s: %w", name, err)
		}
		result = append(result, CICacheResourceDigest{Resource: name, Digest: digest})
	}
	sortCacheResourceDigests(result)
	return result, nil
}

func sortCacheResourceDigests(values []CICacheResourceDigest) {
	sort.Slice(values, func(i, j int) bool { return values[i].Resource < values[j].Resource })
}

func aggregateCacheDigests(values []CICacheResourceDigest) string {
	sortCacheResourceDigests(values)
	payload, _ := json.Marshal(values)
	return "sha256:" + resources.Hash(payload)
}

type cacheDigestPath struct {
	Label string
	Path  string
}

func (builder *ciCacheIdentityBuilder) digestComponents(paths []cacheDigestPath) (string, error) {
	sort.Slice(paths, func(i, j int) bool { return paths[i].Label < paths[j].Label })
	values := make([]CICacheResourceDigest, 0, len(paths))
	for _, component := range paths {
		digest, err := builder.digestPath(component.Path)
		if err != nil {
			return "", err
		}
		values = append(values, CICacheResourceDigest{Resource: component.Label, Digest: digest})
	}
	return aggregateCacheDigests(values), nil
}

func (builder *ciCacheIdentityBuilder) digestPath(path string) (string, error) {
	path = cleanAbs(path)
	if digest, ok := builder.digestCache[path]; ok {
		return digest, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	var paths []string
	if !info.IsDir() {
		paths = []string{path}
	} else if builder.useGitFiles && pathWithin(path, builder.repoRoot) {
		for _, candidate := range builder.gitFiles {
			if pathWithin(candidate, path) && !cachePathPruned(path, candidate) {
				paths = append(paths, candidate)
			}
		}
	} else {
		err = filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if candidate == path {
				return nil
			}
			if entry.IsDir() {
				if cachePathPruned(path, candidate) {
					return filepath.SkipDir
				}
				return nil
			}
			if cacheFilePruned(candidate) {
				return nil
			}
			paths = append(paths, cleanAbs(candidate))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)
	hasher := sha256.New()
	writeCacheRecord(hasher, "root", filepath.Base(path))
	for _, candidate := range paths {
		if err := hashCacheEntry(hasher, path, candidate); err != nil {
			return "", err
		}
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	builder.digestCache[path] = digest
	return digest, nil
}

func cachePathPruned(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		switch part {
		case ".git", ".codefly", ".cache", ".next", ".nuxt", ".output", ".turbo", ".venv", "venv", "__pycache__", ".pytest_cache", "coverage", "node_modules", "target", ".gomodcache":
			return true
		}
	}
	return false
}

func cacheFilePruned(path string) bool {
	name := filepath.Base(path)
	return name == ".DS_Store" || strings.HasSuffix(name, ".tsbuildinfo") || strings.HasSuffix(name, ".log")
}

func hashCacheEntry(hasher hash.Hash, root, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			writeCacheRecord(hasher, "missing", filepath.ToSlash(relative))
			return nil
		}
		return err
	}
	relative := filepath.Base(path)
	if rootInfo, rootErr := os.Lstat(root); rootErr == nil && rootInfo.IsDir() {
		relative, err = filepath.Rel(root, path)
		if err != nil {
			return err
		}
	}
	relative = filepath.ToSlash(relative)
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		writeCacheRecord(hasher, "symlink", relative, filepath.ToSlash(target))
		return nil
	}
	if info.IsDir() {
		writeCacheRecord(hasher, "directory", relative)
		return nil
	}
	if !info.Mode().IsRegular() {
		writeCacheRecord(hasher, "special", relative, info.Mode().String())
		return nil
	}
	executable := "0"
	if info.Mode().Perm()&0o111 != 0 {
		executable = "1"
	}
	writeCacheRecord(hasher, "file", relative, executable, fmt.Sprintf("%d", info.Size()))
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	hasher.Write([]byte{0})
	return errors.Join(copyErr, closeErr)
}

func writeCacheRecord(hasher hash.Hash, fields ...string) {
	var length [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hasher.Write(length[:])
		_, _ = hasher.Write([]byte(field))
	}
	_, _ = hasher.Write([]byte{0xff})
}
