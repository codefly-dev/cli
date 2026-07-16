package ci

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
)

const planSchemaVersion = 1

type PlanOptions struct {
	Base         string
	Head         string
	ChangedFiles []string
	All          bool
	// RepoRoot is primarily useful to provider adapters and tests. Empty asks
	// Git for the repository root and falls back conservatively when unavailable.
	RepoRoot string
}

type Plan struct {
	SchemaVersion   int              `json:"schema_version"`
	Workspace       string           `json:"workspace"`
	Base            string           `json:"base,omitempty"`
	Head            string           `json:"head,omitempty"`
	ChangedFiles    []string         `json:"changed_files"`
	SelectionReason string           `json:"selection_reason,omitempty"`
	Services        []PlannedService `json:"services"`
}

type PlannedService struct {
	Service        string   `json:"service"`
	Classification string   `json:"classification"`
	Reasons        []string `json:"reasons"`
	Paths          []string `json:"paths,omitempty"`
}

type serviceRecord struct {
	unique  string
	module  string
	dir     string
	service *resources.Service
}

type moduleRecord struct {
	name     string
	dir      string
	services []string
}

type mutablePlanService struct {
	classification string
	reasons        []string
	paths          []string
}

func BuildPlan(ctx context.Context, workspace *resources.Workspace, opts PlanOptions) (*Plan, error) {
	if workspace == nil {
		return nil, fmt.Errorf("workspace is nil")
	}
	services, modules, err := loadPlanInventory(ctx, workspace)
	if err != nil {
		return nil, err
	}

	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot == "" {
		repoRoot, err = gitRoot(ctx, workspace.Dir())
		if err != nil {
			repoRoot = workspace.Dir()
		}
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}

	plan := &Plan{
		SchemaVersion: planSchemaVersion,
		Workspace:     workspace.Name,
		Base:          strings.TrimSpace(opts.Base),
		Head:          strings.TrimSpace(opts.Head),
		Services:      []PlannedService{},
		ChangedFiles:  []string{},
	}
	selected := map[string]*mutablePlanService{}

	selectAll := func(classification, reason string) {
		for _, record := range services {
			addPlanSelection(selected, record.unique, classification, reason, "")
		}
	}

	if opts.All {
		plan.SelectionReason = "explicit --all"
		selectAll("global", plan.SelectionReason)
		return finalizePlan(ctx, workspace, plan, services, selected)
	}

	changed := append([]string(nil), opts.ChangedFiles...)
	if len(changed) == 0 {
		if plan.Base == "" && isCIEnvironment() {
			plan.SelectionReason = "CI change bounds were not supplied; selected all services conservatively"
			selectAll("global", plan.SelectionReason)
			return finalizePlan(ctx, workspace, plan, services, selected)
		}
		changed, err = discoverGitChanges(ctx, repoRoot, plan.Base, plan.Head)
		if err != nil {
			plan.SelectionReason = fmt.Sprintf("change discovery failed (%v); selected all services conservatively", err)
			selectAll("global", plan.SelectionReason)
			return finalizePlan(ctx, workspace, plan, services, selected)
		}
	}

	plan.ChangedFiles = normalizeChangedPaths(repoRoot, changed)
	if plan.Head == "" && plan.Base != "" {
		plan.Head = "HEAD"
	}

	libraryConsumers, err := loadLibraryConsumers(ctx, workspace, services)
	if err != nil {
		return nil, err
	}
	for _, changedPath := range plan.ChangedFiles {
		classifyChangedPath(repoRoot, workspace, changedPath, services, modules, libraryConsumers, selected)
	}

	return finalizePlan(ctx, workspace, plan, services, selected)
}

func loadPlanInventory(ctx context.Context, workspace *resources.Workspace) ([]serviceRecord, []moduleRecord, error) {
	mods, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load workspace modules: %w", err)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })

	var services []serviceRecord
	var modules []moduleRecord
	for _, mod := range mods {
		loaded, err := mod.LoadServices(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load services for module %s: %w", mod.Name, err)
		}
		sort.Slice(loaded, func(i, j int) bool { return loaded[i].Name < loaded[j].Name })
		module := moduleRecord{name: mod.Name, dir: cleanAbs(mod.Dir())}
		for _, service := range loaded {
			service.WithModule(mod.Name)
			identity, err := service.Identity()
			if err != nil {
				return nil, nil, fmt.Errorf("identify service %s/%s: %w", mod.Name, service.Name, err)
			}
			record := serviceRecord{
				unique:  identity.Unique(),
				module:  mod.Name,
				dir:     cleanAbs(service.Dir()),
				service: service,
			}
			services = append(services, record)
			module.services = append(module.services, record.unique)
		}
		modules = append(modules, module)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].unique < services[j].unique })
	return services, modules, nil
}

func loadLibraryConsumers(ctx context.Context, workspace *resources.Workspace, services []serviceRecord) (map[string][]string, error) {
	libraries, err := workspace.LoadLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace libraries: %w", err)
	}

	// dependency -> dependent library, matching the service graph orientation.
	libDependents := map[string][]string{}
	for _, library := range libraries {
		for _, dependency := range library.LibraryDeps {
			if dependency != nil && dependency.Name != "" {
				libDependents[dependency.Name] = append(libDependents[dependency.Name], library.Name)
			}
		}
	}

	directConsumers := map[string][]string{}
	for _, record := range services {
		for _, dependency := range record.service.LibraryDependencies {
			if dependency != nil && dependency.Name != "" {
				directConsumers[dependency.Name] = append(directConsumers[dependency.Name], record.unique)
			}
		}
	}

	result := map[string][]string{}
	for _, library := range libraries {
		seen := map[string]bool{}
		queue := []string{library.Name}
		for len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			if seen[name] {
				continue
			}
			seen[name] = true
			queue = append(queue, libDependents[name]...)
		}
		var consumers []string
		for name := range seen {
			consumers = append(consumers, directConsumers[name]...)
		}
		result[library.Name] = sortedUnique(consumers)
	}
	return result, nil
}

func classifyChangedPath(repoRoot string, workspace *resources.Workspace, changedPath string, services []serviceRecord, modules []moduleRecord, libraryConsumers map[string][]string, selected map[string]*mutablePlanService) {
	absPath := changedPath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(repoRoot, filepath.FromSlash(changedPath))
	}
	absPath = cleanAbs(absPath)
	workspaceDir := cleanAbs(workspace.Dir())

	if isDocumentationOrProviderPath(changedPath) {
		return
	}

	for _, record := range services {
		if pathWithin(absPath, record.dir) {
			addPlanSelection(selected, record.unique, "direct", "service input changed", changedPath)
			return
		}
	}

	librariesDir := filepath.Join(workspaceDir, "libraries")
	if pathWithin(absPath, librariesDir) {
		rel, err := filepath.Rel(librariesDir, absPath)
		if err == nil {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) > 0 && parts[0] != "." && parts[0] != "" {
				library := parts[0]
				consumers, known := libraryConsumers[library]
				if known {
					for _, service := range consumers {
						addPlanSelection(selected, service, "direct", "consumes changed library "+library, changedPath)
					}
					return
				}
			}
		}
		// ChangedFiles already carries the complete global input set. Do not
		// duplicate every global path into every selected service record.
		selectAllRecords(selected, services, "global", "unclassified library change", "")
		return
	}

	if absPath == filepath.Join(workspaceDir, resources.WorkspaceConfigurationName) ||
		pathWithin(absPath, filepath.Join(workspaceDir, "configurations")) ||
		pathWithin(absPath, filepath.Join(workspaceDir, "environments")) {
		selectAllRecords(selected, services, "global", "workspace-level input changed", "")
		return
	}

	for _, module := range modules {
		if pathWithin(absPath, module.dir) {
			for _, service := range module.services {
				addPlanSelection(selected, service, "direct", "module-level input changed", changedPath)
			}
			return
		}
	}

	if pathWithin(absPath, workspaceDir) {
		selectAllRecords(selected, services, "global", "workspace-level input changed", "")
		return
	}

	// Provider metadata and documentation were handled above. Any other change
	// outside a nested workspace can affect shared build/configuration inputs.
	selectAllRecords(selected, services, "global", "unclassified repository input changed", "")
}

func finalizePlan(ctx context.Context, workspace *resources.Workspace, plan *Plan, services []serviceRecord, selected map[string]*mutablePlanService) (*Plan, error) {
	if len(selected) == 0 {
		plan.ChangedFiles = sortedUnique(plan.ChangedFiles)
		return plan, nil
	}

	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("load service dependency graph: %w", err)
	}
	graph := dependencies.Graph()

	direct := make([]string, 0, len(selected))
	for unique, service := range selected {
		// Global changes already selected the complete inventory. Expanding each
		// global service would only add noisy "depends on" reasons.
		if service.classification == "direct" {
			direct = append(direct, unique)
		}
	}
	sort.Strings(direct)
	for _, origin := range direct {
		if !graph.HasNode(origin) {
			continue
		}
		subgraph, err := graph.SubgraphFrom(origin)
		if err != nil {
			return nil, fmt.Errorf("expand dependents of %s: %w", origin, err)
		}
		for _, node := range subgraph.Nodes() {
			if node.ID == origin {
				continue
			}
			addPlanSelection(selected, node.ID, "dependent", "depends on "+origin, "")
		}
	}

	order, err := graph.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("sort affected service graph: %w", err)
	}
	known := map[string]bool{}
	for _, unique := range order {
		service, ok := selected[unique]
		if !ok {
			continue
		}
		known[unique] = true
		plan.Services = append(plan.Services, PlannedService{
			Service:        unique,
			Classification: service.classification,
			Reasons:        sortedUnique(service.reasons),
			Paths:          sortedUnique(service.paths),
		})
	}
	// A malformed/incomplete dependency graph must not silently drop a loaded
	// service. Append any selected inventory record deterministically.
	for _, record := range services {
		if known[record.unique] {
			continue
		}
		service, ok := selected[record.unique]
		if !ok {
			continue
		}
		plan.Services = append(plan.Services, PlannedService{
			Service:        record.unique,
			Classification: service.classification,
			Reasons:        sortedUnique(service.reasons),
			Paths:          sortedUnique(service.paths),
		})
	}
	plan.ChangedFiles = sortedUnique(plan.ChangedFiles)
	return plan, nil
}

func addPlanSelection(selected map[string]*mutablePlanService, unique, classification, reason, path string) {
	service := selected[unique]
	if service == nil {
		service = &mutablePlanService{classification: classification}
		selected[unique] = service
	}
	if classificationRank(classification) > classificationRank(service.classification) {
		service.classification = classification
		if classification == "global" {
			service.reasons = nil
			service.paths = nil
		}
	}
	// Once a global input selected a service, resource-specific details are
	// redundant and can make plans enormous in a dirty workspace.
	if service.classification == "global" && classification != "global" {
		return
	}
	if reason != "" {
		service.reasons = append(service.reasons, reason)
	}
	if path != "" {
		service.paths = append(service.paths, path)
	}
}

func classificationRank(classification string) int {
	switch classification {
	case "global":
		return 3
	case "direct":
		return 2
	case "dependent":
		return 1
	default:
		return 0
	}
}

func selectAllRecords(selected map[string]*mutablePlanService, services []serviceRecord, classification, reason, path string) {
	for _, record := range services {
		addPlanSelection(selected, record.unique, classification, reason, path)
	}
}

func gitRoot(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git repository root: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("git returned an empty repository root")
	}
	return root, nil
}

func discoverGitChanges(ctx context.Context, repoRoot, base, head string) ([]string, error) {
	if base != "" {
		if head == "" {
			head = "HEAD"
		}
		out, err := gitOutput(ctx, repoRoot, "diff", "--name-status", "-z", "--find-renames", base, head)
		if err != nil {
			return nil, err
		}
		return parseNameStatusZ(out), nil
	}

	out, err := gitOutput(ctx, repoRoot, "diff", "--name-status", "-z", "--find-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	changed := parseNameStatusZ(out)
	untracked, err := gitOutput(ctx, repoRoot, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, path := range bytes.Split(untracked, []byte{0}) {
		if len(path) > 0 {
			changed = append(changed, string(path))
		}
	}
	return sortedUnique(changed), nil
}

func gitOutput(ctx context.Context, repoRoot string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func parseNameStatusZ(out []byte) []string {
	fields := bytes.Split(out, []byte{0})
	var paths []string
	for i := 0; i < len(fields); {
		if len(fields[i]) == 0 {
			i++
			continue
		}
		status := string(fields[i])
		i++
		if i >= len(fields) {
			break
		}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			paths = append(paths, string(fields[i]))
			i++
			if i < len(fields) {
				paths = append(paths, string(fields[i]))
				i++
			}
			continue
		}
		paths = append(paths, string(fields[i]))
		i++
	}
	return sortedUnique(paths)
}

func normalizeChangedPaths(repoRoot string, paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if filepath.IsAbs(path) {
			if rel, err := filepath.Rel(repoRoot, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				path = rel
			}
		}
		path = filepath.ToSlash(filepath.Clean(path))
		result = append(result, strings.TrimPrefix(path, "./"))
	}
	return sortedUnique(result)
}

func pathWithin(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func cleanAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)

	// Workspaces commonly keep a stable Codefly layout behind symlinks (for
	// example modules/name -> ../module). Git and CI providers report the real
	// repository path, while resource loading sees the layout path. Resolve the
	// longest existing prefix so both existing and deleted changed files map to
	// the same resource directory.
	current := abs
	var missing []string
	for {
		if resolved, resolveErr := filepath.EvalSymlinks(current); resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func isDocumentationOrProviderPath(path string) bool {
	path = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
	lower := strings.ToLower(path)
	if strings.HasPrefix(lower, ".github/") || lower == ".github" {
		return true
	}
	parts := strings.Split(lower, "/")
	for _, part := range parts[:max(0, len(parts)-1)] {
		if part == "docs" || part == "documentation" {
			return true
		}
	}
	base := filepath.Base(lower)
	ext := strings.ToLower(filepath.Ext(base))
	if ext == ".md" || ext == ".mdx" || ext == ".rst" {
		return true
	}
	return base == "license" || strings.HasPrefix(base, "license.") || base == "codeowners"
}

func isCIEnvironment() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("CI")))
	return value != "" && value != "0" && value != "false" && value != "no"
}
