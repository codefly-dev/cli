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
	"sync"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
)

const (
	cacheIdentitySchemaVersion = 2
	cacheIdentityAlgorithm     = "sha256"
	cacheStatusIdentityOnly    = "identity_only"
	cacheStatusUnavailable     = "unavailable"
	cacheStatusIneligible      = "ineligible"
	cacheStatusMiss            = "miss"
	cacheStatusHit             = "hit"
)

// CICacheIdentity is a content-addressed description of a CI task. Schema
// version 2 binds every repository byte that is not attributed to an unrelated
// resource, so a matching key cannot hide an undeclared input. Status is this
// run's reuse outcome for the task; Stored is the separate question of whether
// this run published its own result for a later one.
type CICacheIdentity struct {
	SchemaVersion int                  `json:"schema_version"`
	Algorithm     string               `json:"algorithm"`
	Key           string               `json:"key,omitempty"`
	Status        string               `json:"status"`
	StatusReason  string               `json:"status_reason,omitempty"`
	Stored        bool                 `json:"stored"`
	Inputs        CICacheIdentityInput `json:"inputs"`
	Limitations   []string             `json:"limitations,omitempty"`
	Reuse         *CacheReuse          `json:"reuse,omitempty"`
}

// CacheReuse records which previously verified execution a reused task stands
// in for. It is written only after the record's signature, input identity and
// every restored artifact digest have been verified.
type CacheReuse struct {
	Reference  string             `json:"reference"`
	Run        string             `json:"run"`
	Revision   string             `json:"revision,omitempty"`
	Identity   string             `json:"identity"`
	RecordedAt string             `json:"recorded_at"`
	Artifacts  []CIReportArtifact `json:"artifacts,omitempty"`
}

type CICacheIdentityInput struct {
	CodeflyVersion  string                  `json:"codefly_version"`
	CoreVersion     string                  `json:"core_version"`
	Platform        string                  `json:"platform"`
	RuntimeContext  string                  `json:"runtime_context"`
	Phase           string                  `json:"phase"`
	Suite           string                  `json:"suite,omitempty"`
	Service         string                  `json:"service"`
	Environment     string                  `json:"environment"`
	Agent           CICacheAgentInput       `json:"agent"`
	CLIDigest       string                  `json:"cli_digest,omitempty"`
	RepositoryRest  string                  `json:"repository_rest_digest,omitempty"`
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
	environment    string
	repoRoot       string
	gitFiles       []string
	useGitFiles    bool
	resourceRoots  []string
	digestCache    map[string]string
	agentInputs    map[string]ciCacheAgentResolution
}

// ciCacheAgentResolution memoizes one pinned agent's binding, so a workspace
// whose services share a pin resolves and installs it once per run.
type ciCacheAgentResolution struct {
	input       CICacheAgentInput
	limitations []string
}

func newCICacheIdentityBuilder(ctx context.Context, workspace *resources.Workspace, codeflyVersion, environment string) *ciCacheIdentityBuilder {
	builder := &ciCacheIdentityBuilder{
		workspace:      workspace,
		codeflyVersion: strings.TrimSpace(codeflyVersion),
		environment:    strings.TrimSpace(environment),
		digestCache:    map[string]string{},
		agentInputs:    map[string]ciCacheAgentResolution{},
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
	roots, err := attributedResourceRoots(ctx, workspace)
	if err != nil {
		return builder
	}
	builder.repoRoot = cleanAbs(repoRoot)
	builder.gitFiles = files
	builder.resourceRoots = roots
	builder.useGitFiles = true
	return builder
}

// attributedResourceRoots lists the directories whose bytes an identity already
// attributes to a named service or library. Everything else a repository tracks
// is unattributed: it is hashed as one remainder so no file can change without
// changing some task identity.
func attributedResourceRoots(ctx context.Context, workspace *resources.Workspace) ([]string, error) {
	services, _, err := loadPlanInventory(ctx, workspace)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(services))
	for _, service := range services {
		roots = append(roots, service.dir)
	}
	libraries, err := workspace.LoadLibraries(ctx)
	if err != nil {
		return nil, err
	}
	for _, library := range libraries {
		roots = append(roots, cleanAbs(library.Dir()))
	}
	return sortedUnique(roots), nil
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
			Environment:    builder.environment,
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
			Environment:    builder.environment,
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
	if err == nil {
		identity.Inputs.CLIDigest, identity.Inputs.RepositoryRest, identity.Limitations, err = builder.ambientInputs()
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
	agentInput, agentLimitations := builder.agentInput(ctx, service.Agent)
	inputs.Agent = agentInput
	cliDigest, repositoryRest, limitations, err := builder.ambientInputs()
	limitations = append(limitations, agentLimitations...)
	if err != nil {
		return inputs, limitations, err
	}
	inputs.CLIDigest = cliDigest
	inputs.RepositoryRest = repositoryRest

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

// installCacheAgent installs a pinned agent release into the local cache. It is
// a seam so identity tests never reach a registry.
var installCacheAgent = manager.Download

// agentInput binds the pinned agent release the task executes against,
// installing it when this machine does not have it yet. The release is fully
// determined before any task runs, so a digest that only exists once the agent
// has been spawned here would leave a fresh machine permanently ineligible for
// reuse. Installing it is not extra work: an executed task downloads the same
// binary moments later.
func (builder *ciCacheIdentityBuilder) agentInput(ctx context.Context, pinned *resources.Agent) (CICacheAgentInput, []string) {
	if resolution, ok := builder.agentInputs[pinned.Unique()]; ok {
		return resolution.input, resolution.limitations
	}
	agent := *pinned
	input, limitations := builder.resolveAgentInput(ctx, &agent)
	builder.agentInputs[pinned.Unique()] = ciCacheAgentResolution{input: input, limitations: limitations}
	return input, limitations
}

func (builder *ciCacheIdentityBuilder) resolveAgentInput(ctx context.Context, agent *resources.Agent) (CICacheAgentInput, []string) {
	if _, err := resolveAgentLatest(ctx, agent); err != nil {
		return cacheAgentInput(agent), []string{"pinned agent version cannot be resolved"}
	}
	input := cacheAgentInput(agent)
	path, err := agent.Path(ctx)
	if err != nil {
		return input, []string{"resolved agent binary path is unavailable"}
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return input, []string{"resolved agent binary cannot be inspected"}
		}
		if installErr := installCacheAgent(ctx, agent); installErr != nil {
			return input, []string{fmt.Sprintf("pinned agent %s cannot be installed: %v", agent.Identifier(), installErr)}
		}
	}
	input.Digest, err = builder.digestPath(path)
	if err != nil {
		return input, []string{"resolved agent binary cannot be hashed"}
	}
	return input, nil
}

func cacheAgentInput(agent *resources.Agent) CICacheAgentInput {
	return CICacheAgentInput{
		Kind:      string(agent.Kind),
		Publisher: agent.Publisher,
		Name:      agent.Name,
		Version:   agent.Version,
	}
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

// ambientInputs binds the two execution inputs that belong to no single
// resource: the CLI binary that drives every agent, and every tracked
// repository byte that is not attributed to a named service or library.
func (builder *ciCacheIdentityBuilder) ambientInputs() (string, string, []string, error) {
	var limitations []string
	digest, err := cachedCLIDigest()
	if err != nil {
		limitations = append(limitations, "running CLI binary cannot be hashed")
	}
	rest, err := builder.repositoryRestDigest()
	if err != nil {
		return digest, "", limitations, fmt.Errorf("hash unattributed repository inputs: %w", err)
	}
	if rest == "" {
		limitations = append(limitations, "unattributed repository inputs are unavailable without Git")
	}
	return digest, rest, limitations, nil
}

func (builder *ciCacheIdentityBuilder) repositoryRestDigest() (string, error) {
	if !builder.useGitFiles {
		return "", nil
	}
	if digest, ok := builder.digestCache[repositoryRestCacheEntry]; ok {
		return digest, nil
	}
	hasher := sha256.New()
	writeCacheRecord(hasher, "root", "repository")
	for _, candidate := range builder.gitFiles {
		if cachePathPruned(builder.repoRoot, candidate) || cacheFilePruned(candidate) {
			continue
		}
		if withinAttributedResource(candidate, builder.resourceRoots) {
			continue
		}
		if err := hashCacheEntry(hasher, builder.repoRoot, candidate); err != nil {
			return "", err
		}
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	builder.digestCache[repositoryRestCacheEntry] = digest
	return digest, nil
}

// repositoryRestCacheEntry is not a filesystem path, so it cannot collide with
// the absolute paths digestPath caches.
const repositoryRestCacheEntry = "repository-rest"

// withinAttributedResource is pathWithin specialized to the remainder scan,
// which compares every tracked file against every resource root. Both sides are
// already cleaned absolute paths, so a prefix test avoids the relative-path
// allocation that scan would otherwise repeat millions of times in a large
// workspace.
func withinAttributedResource(path string, roots []string) bool {
	for _, root := range roots {
		if strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

var cachedCLIDigest = sync.OnceValues(func() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
})

// reuseEligibility reports whether a verified successful result may stand in for
// executing this task. Every input a rerun would consume must be bound by the
// key; anything Codefly could not resolve keeps the task executing.
func (identity *CICacheIdentity) reuseEligibility() (bool, string) {
	switch {
	case identity.SchemaVersion != cacheIdentitySchemaVersion:
		return false, "identity schema is not the reuse contract version"
	case identity.Key == "":
		return false, "identity is unavailable"
	case len(identity.Limitations) > 0:
		return false, "identity has unresolved inputs: " + strings.Join(identity.Limitations, "; ")
	case identity.Inputs.Environment == "":
		return false, "no execution environment identity was declared"
	case identity.Inputs.RepositoryRest == "":
		return false, "unattributed repository inputs are unbound"
	case identity.Inputs.CLIDigest == "":
		return false, "the running CLI binary is unbound"
	case identity.Inputs.Agent.Digest == "":
		return false, "the resolved agent binary is unbound"
	}
	return true, ""
}
