package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

var (
	scpGitURLPattern     = regexp.MustCompile(`^git@github\.com:([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?$`)
	githubSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	pathComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

const (
	httpsScheme   = "https"
	sshScheme     = "ssh"
	jsonExtension = ".json"
	yamlExtension = ".yaml"
	ymlExtension  = ".yml"
)

type preparedRepository struct {
	dir     string
	cleanup func()
	plan    PublishPlan
}

type repositoryConfig struct {
	RepoURL      string
	FetchRepoURL string
	Path         string
	Branch       string
}

func PlanPublish(ctx context.Context, workspace *resources.Workspace, request *PublishRequest) (PublishPlan, error) {
	prepared, err := preparePublish(ctx, workspace, request, "", false)
	if err != nil {
		return PublishPlan{}, err
	}
	defer prepared.cleanup()
	return prepared.plan, nil
}

func Publish(ctx context.Context, workspace *resources.Workspace, mutation *PublishMutation, permit mutationauthority.PreparedPermit) (PublishResult, error) {
	if err := permit.Validate(); err != nil {
		return PublishResult{}, err
	}
	if mutation.PlanID == "" {
		return PublishResult{}, fmt.Errorf("publish requires an inspected plan ID")
	}
	inspected, err := preparePublish(ctx, workspace, &mutation.Request, "", false)
	if err != nil {
		return PublishResult{}, err
	}
	if inspected.plan.ID != mutation.PlanID {
		current := inspected.plan.ID
		inspected.cleanup()
		return PublishResult{}, fmt.Errorf("publish plan is stale: prepared %s, current %s", mutation.PlanID, current)
	}
	inspected.cleanup()
	prepared, err := preparePublish(ctx, workspace, &mutation.Request, "", true)
	if err != nil {
		return PublishResult{}, err
	}
	defer prepared.cleanup()
	if prepared.plan.ID != mutation.PlanID {
		return PublishResult{}, fmt.Errorf("publish plan changed while advertising its snapshot: prepared %s, current %s", mutation.PlanID, prepared.plan.ID)
	}
	return commitAndPublish(ctx, workspace, prepared, &mutation.Request)
}

func PlanRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest) (RollbackPlan, error) {
	prepared, revision, err := prepareRollback(ctx, workspace, request)
	if err != nil {
		return RollbackPlan{}, err
	}
	defer prepared.cleanup()
	return RollbackPlan{PublishPlan: prepared.plan, ToRevision: revision}, nil
}

func Rollback(ctx context.Context, workspace *resources.Workspace, mutation *RollbackMutation, permit mutationauthority.PreparedPermit) (PublishResult, error) {
	if err := permit.Validate(); err != nil {
		return PublishResult{}, err
	}
	if mutation.PlanID == "" {
		return PublishResult{}, fmt.Errorf("rollback requires an inspected plan ID")
	}
	prepared, _, err := prepareRollback(ctx, workspace, &mutation.Request)
	if err != nil {
		return PublishResult{}, err
	}
	defer prepared.cleanup()
	if prepared.plan.ID != mutation.PlanID {
		return PublishResult{}, fmt.Errorf("rollback plan is stale: prepared %s, current %s", mutation.PlanID, prepared.plan.ID)
	}
	request := mutation.Request.PublishRequest
	if request.CommitMessage == "" {
		request.CommitMessage = "Re-promote " + mutation.Request.Module + " from " + mutation.Request.ToRevision
	}
	return commitAndPublish(ctx, workspace, prepared, &request)
}

func preparePublish(
	ctx context.Context,
	workspace *resources.Workspace,
	request *PublishRequest,
	restoreRevision string,
	publishSnapshot bool,
) (*preparedRepository, error) {
	if err := validatePublishRequest(workspace, request); err != nil {
		return nil, err
	}
	config, repositorySlug, baseBranch, pathRoot, err := resolveGitops(workspace, request.Environment, request.Local)
	if err != nil {
		return nil, err
	}
	localFetchHost, err := localFetchRemoteHost(workspace, request, config)
	if err != nil {
		return nil, err
	}
	rendered := filepath.Join(workspace.Dir(), "deployments", "modules", request.Module)
	var inventory Inventory
	if restoreRevision == "" {
		inventory, err = loadPublicationInventory(ctx, workspace, request, rendered, pathRoot)
		if err != nil {
			return nil, err
		}
	}

	promotionBranch := request.PromotionBranch
	if promotionBranch == "" {
		promotionBranch = "codefly/promote-" + sanitizeRef(request.Module) + "-" + sanitizeRef(request.Environment)
	}
	repo, cleanup, baseRevision, branchRevision, err := clonePromotionRepository(ctx, config.RepoURL, baseBranch, promotionBranch)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*preparedRepository, error) {
		cleanup()
		return nil, err
	}
	targetPath := filepath.ToSlash(filepath.Join(pathRoot, "deployments", "modules", request.Module))
	target, err := confinedJoin(repo, targetPath)
	if err != nil {
		return fail(err)
	}
	if branchRevision != "" {
		existing, err := changedPathsBetween(ctx, repo, baseRevision, branchRevision)
		if err != nil {
			return fail(err)
		}
		for _, changed := range existing {
			if changed != targetPath && !strings.HasPrefix(changed, targetPath+"/") {
				return fail(fmt.Errorf("promotion branch %s contains unrelated change %s", promotionBranch, changed))
			}
		}
	}
	startRevision := baseRevision
	if branchRevision != "" {
		startRevision = branchRevision
	}
	var snapshotRevision string
	if restoreRevision == "" {
		module, err := workspace.LoadModuleFromName(ctx, request.Module)
		if err != nil {
			return fail(fmt.Errorf("load rendered module %q: %w", request.Module, err))
		}
		snapshotRevision, inventory, err = prepareServicePublication(
			ctx,
			repo,
			target,
			targetPath,
			rendered,
			&inventory,
			workspace,
			module,
			request.Environment,
			config,
			promotionBranch,
			publishSnapshot,
			localFetchHost,
		)
		if err != nil {
			return fail(err)
		}
	} else {
		if err := restoreCloneTree(ctx, repo, targetPath, restoreRevision); err != nil {
			return fail(err)
		}
		snapshotRevision, err = bootstrapRevision(filepath.Join(target, "bootstrap"))
		if err != nil {
			return fail(err)
		}
		if snapshotRevision == "" {
			snapshotRevision = restoreRevision
		}
		if err := ValidateRenderedTree(target, "", true); err != nil {
			return fail(fmt.Errorf("validate rollback render: %w", err))
		}
		inventory, err = LoadInventory(target)
		if err != nil {
			return fail(err)
		}
	}
	if _, err := gitCommand(ctx, repo, "add", "-A", "--", targetPath); err != nil {
		return fail(err)
	}
	changed, err := stagedPathsSince(ctx, repo, startRevision, targetPath)
	if err != nil {
		return fail(err)
	}
	if len(changed) == 0 {
		if branchRevision == "" {
			return fail(fmt.Errorf("promotion has no changes"))
		}
	}
	diff, err := gitCommand(ctx, repo, "diff", "--cached", "--binary", startRevision, "--", targetPath)
	if err != nil {
		return fail(err)
	}
	plan := PublishPlan{
		Repository: config.RepoURL, RepositorySlug: repositorySlug,
		Path: targetPath, BaseBranch: baseBranch, BaseRevision: baseRevision,
		PromotionBranch: promotionBranch, BranchRevision: branchRevision,
		ExistingCommit: branchRevision,
		Module:         request.Module, Environment: request.Environment,
		RenderDigest: inventory.Digest, SnapshotRevision: snapshotRevision,
		Changed: changed, Diff: diff,
	}
	plan.ID, err = publishPlanID(&plan, restoreRevision)
	if err != nil {
		return fail(err)
	}
	return &preparedRepository{
		dir: repo, cleanup: cleanup, plan: plan,
	}, nil
}

func loadPublicationInventory(
	ctx context.Context,
	workspace *resources.Workspace,
	request *PublishRequest,
	rendered,
	pathRoot string,
) (Inventory, error) {
	if err := ValidateRenderedTree(rendered, "", true); err != nil {
		return Inventory{}, fmt.Errorf("validate promotable render: %w", err)
	}
	inventory, err := LoadInventory(rendered)
	if err != nil {
		return Inventory{}, err
	}
	if inventory.Module != request.Module || inventory.Environment != request.Environment || inventory.Unit != "" {
		return Inventory{}, fmt.Errorf(
			"render inventory targets module %q environment %q unit %q",
			inventory.Module,
			inventory.Environment,
			inventory.Unit,
		)
	}
	expectedOwnedPath := filepath.ToSlash(filepath.Join(pathRoot, "deployments", "modules", request.Module))
	if inventory.OwnedPath != expectedOwnedPath {
		return Inventory{}, fmt.Errorf("render inventory owns path %q, expected %q", inventory.OwnedPath, expectedOwnedPath)
	}
	if err := validateModuleUnits(
		ctx,
		workspace,
		request.Module,
		request.Environment,
		inventory.Units,
	); err != nil {
		return Inventory{}, err
	}
	return inventory, nil
}

func validateModuleUnits(
	ctx context.Context,
	workspace *resources.Workspace,
	moduleName string,
	environment string,
	rendered []InventoryUnit,
) error {
	module, err := workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return fmt.Errorf("load rendered module %q: %w", moduleName, err)
	}
	selectedEnvironment, err := orchestration.SelectEnvironment(workspace, environment)
	if err != nil {
		return err
	}
	managed := selectedEnvironment.ManagedServices
	declared := make([]string, 0, len(module.ServiceReferences))
	for _, reference := range module.ServiceReferences {
		declared = append(declared, reference.Name)
	}
	sort.Strings(declared)
	actual := make([]string, 0, len(rendered))
	for _, unit := range rendered {
		if unit.Module != moduleName {
			return fmt.Errorf("rendered unit %q belongs to module %q, expected %q", unit.Name, unit.Module, moduleName)
		}
		_, expectedManaged := managed[unit.Name]
		if unit.Managed != expectedManaged {
			return fmt.Errorf("rendered unit %q managed state differs from environment %q", unit.Name, environment)
		}
		actual = append(actual, unit.Name)
	}
	sort.Strings(actual)
	if len(actual) != len(declared) {
		return fmt.Errorf("rendered unit graph %v differs from module service graph %v", actual, declared)
	}
	for index := range declared {
		if actual[index] != declared[index] {
			return fmt.Errorf("rendered unit graph %v differs from module service graph %v", actual, declared)
		}
	}
	return nil
}

func prepareServicePublication(
	ctx context.Context,
	repo string,
	target string,
	targetPath string,
	rendered string,
	renderedInventory *Inventory,
	workspace *resources.Workspace,
	module *resources.Module,
	environment string,
	config *repositoryConfig,
	promotionBranch string,
	publishSnapshot bool,
	localFetchHost string,
) (string, Inventory, error) {
	snapshot, err := prepareServiceSnapshot(
		ctx,
		repo,
		target,
		targetPath,
		rendered,
		renderedInventory,
		module,
		environment,
		publishSnapshot,
	)
	if err != nil {
		return "", Inventory{}, err
	}
	unitDirs, err := inventoryUnitDirectories(renderedInventory)
	if err != nil {
		return "", Inventory{}, err
	}
	if err = removePublicationRemainder(target, unitDirs); err != nil {
		return "", Inventory{}, err
	}
	if module.Agent != nil {
		if err := generateArgoBootstrap(
			ctx,
			config,
			target,
			targetPath,
			renderedInventory,
			environment,
			snapshot.revision,
			localFetchHost,
		); err != nil {
			return "", Inventory{}, err
		}
	} else {
		renderedBootstrap := filepath.Join(rendered, "bootstrap")
		if info, statErr := os.Stat(renderedBootstrap); statErr == nil && info.IsDir() {
			if err := copyTree(renderedBootstrap, filepath.Join(target, "bootstrap")); err != nil {
				return "", Inventory{}, fmt.Errorf("stage rendered bootstrap: %w", err)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", Inventory{}, fmt.Errorf("inspect rendered bootstrap: %w", statErr)
		}
	}
	if err = verifyServiceSnapshotBinding(ctx, repo, snapshot.revision, snapshot.servicePaths); err != nil {
		return "", Inventory{}, err
	}
	if err := validateBootstrapRevision(filepath.Join(target, "bootstrap"), snapshot.revision); err != nil {
		return "", Inventory{}, err
	}
	if module.Agent != nil {
		if err = validateBootstrapUnits(
			filepath.Join(target, "bootstrap"),
			targetPath,
			renderedInventory,
			environment,
		); err != nil {
			return "", Inventory{}, err
		}
	}

	options := &RenderOptions{
		Module:               renderedInventory.Module,
		UnitNames:            snapshot.services,
		OwnedPath:            targetPath,
		ModulePath:           renderedInventory.ModulePath,
		Units:                renderedInventory.Units,
		Environment:          renderedInventory.Environment,
		Namespace:            renderedInventory.Namespace,
		AppProject:           renderedInventory.AppProject,
		Promotable:           true,
		CheckUnitDirectories: true,
	}
	if _, err := validateTree(target, options); err != nil {
		return "", Inventory{}, fmt.Errorf("validate generated publication: %w", err)
	}
	finalInventory, err := buildInventory(target, options)
	if err != nil {
		return "", Inventory{}, err
	}
	if err := writeCanonicalInventory(filepath.Join(target, InventoryFilename), &finalInventory); err != nil {
		return "", Inventory{}, err
	}
	if err := ValidateRenderedTree(target, renderedInventory.AppProject, true); err != nil {
		return "", Inventory{}, fmt.Errorf("validate generated publication inventory: %w", err)
	}
	return snapshot.revision, finalInventory, nil
}

type serviceSnapshotPreparation struct {
	revision     string
	services     []string
	servicePaths []string
}

func prepareServiceSnapshot(
	ctx context.Context,
	repo,
	target,
	targetPath,
	rendered string,
	renderedInventory *Inventory,
	module *resources.Module,
	environment string,
	publishSnapshot bool,
) (serviceSnapshotPreparation, error) {
	unitDirs, dirErr := inventoryUnitDirectories(renderedInventory)
	if dirErr != nil {
		return serviceSnapshotPreparation{}, dirErr
	}
	if len(unitDirs) == 0 {
		return serviceSnapshotPreparation{}, fmt.Errorf("rendered module contains no unit snapshot")
	}
	servicePaths := make([]string, 0, len(unitDirs))
	for _, directory := range unitDirs {
		renderedUnitDir := filepath.Join(rendered, directory)
		if info, statErr := os.Stat(renderedUnitDir); statErr != nil || !info.IsDir() {
			return serviceSnapshotPreparation{}, fmt.Errorf("rendered module contains no %s snapshot", directory)
		}
		unitPath := filepath.ToSlash(filepath.Join(targetPath, directory))
		if err := replaceCloneTree(renderedUnitDir, repo, unitPath); err != nil {
			return serviceSnapshotPreparation{}, fmt.Errorf("stage rendered %s: %w", directory, err)
		}
		servicePaths = append(servicePaths, unitPath)
	}
	if renderedInventory.ModulePath != "" {
		renderedModule := filepath.Join(rendered, filepath.FromSlash(renderedInventory.ModulePath))
		modulePath := filepath.ToSlash(filepath.Join(targetPath, renderedInventory.ModulePath))
		if err := replaceCloneTree(renderedModule, repo, modulePath); err != nil {
			return serviceSnapshotPreparation{}, fmt.Errorf("stage rendered module resources: %w", err)
		}
	}
	if _, err := gitCommand(ctx, repo, append([]string{"add", "-A", "--"}, servicePaths...)...); err != nil {
		return serviceSnapshotPreparation{}, err
	}
	existingSnapshot, err := existingServiceSnapshot(ctx, repo, module.Name, environment, filepath.Join(target, "bootstrap"))
	if err != nil {
		return serviceSnapshotPreparation{}, err
	}
	if err = removePublicationRemainder(target, unitDirs); err != nil {
		return serviceSnapshotPreparation{}, err
	}
	serviceNames := inventoryUnitNames(renderedInventory.Units)
	snapshotOptions := &RenderOptions{
		Module:      renderedInventory.Module,
		UnitNames:   serviceNames,
		OwnedPath:   targetPath,
		ModulePath:  renderedInventory.ModulePath,
		Units:       renderedInventory.Units,
		Environment: renderedInventory.Environment,
		Namespace:   renderedInventory.Namespace,
		AppProject:  renderedInventory.AppProject,
		Promotable:  true,
	}
	snapshotInventory, err := buildInventory(target, snapshotOptions)
	if err != nil {
		return serviceSnapshotPreparation{}, err
	}
	if err := writeCanonicalInventory(filepath.Join(target, InventoryFilename), &snapshotInventory); err != nil {
		return serviceSnapshotPreparation{}, fmt.Errorf("write service snapshot inventory: %w", err)
	}
	if err := ValidateServiceSnapshot(target); err != nil {
		return serviceSnapshotPreparation{}, fmt.Errorf("validate service snapshot: %w", err)
	}
	if _, err := gitCommand(ctx, repo, "add", "-A", "--", targetPath); err != nil {
		return serviceSnapshotPreparation{}, err
	}
	snapshotChanged := existingSnapshot == ""
	if !snapshotChanged {
		snapshotChanges, diffErr := stagedPathsSince(ctx, repo, existingSnapshot, targetPath)
		if diffErr != nil {
			return serviceSnapshotPreparation{}, diffErr
		}
		snapshotChanged = len(snapshotChanges) > 0
	}
	lineageMissing := false
	if existingSnapshot != "" {
		lineageMissing, err = snapshotLineageMissing(ctx, repo, existingSnapshot)
		if err != nil {
			return serviceSnapshotPreparation{}, err
		}
	}
	snapshotRevision := existingSnapshot
	if snapshotChanged || lineageMissing {
		snapshotRevision, err = commitServiceSnapshot(ctx, repo, module.Name, environment, existingSnapshot)
		if err != nil {
			return serviceSnapshotPreparation{}, err
		}
	}
	if publishSnapshot {
		if err := publishServiceSnapshot(ctx, repo, module.Name, environment, snapshotRevision); err != nil {
			return serviceSnapshotPreparation{}, err
		}
	}
	return serviceSnapshotPreparation{
		revision:     snapshotRevision,
		services:     serviceNames,
		servicePaths: servicePaths,
	}, nil
}

func verifyServiceSnapshotBinding(ctx context.Context, repo, snapshotRevision string, servicePaths []string) error {
	if _, err := gitCommand(ctx, repo, append([]string{"add", "-A", "--"}, servicePaths...)...); err != nil {
		return err
	}
	changed, err := stagedPathsSince(ctx, repo, snapshotRevision, servicePaths...)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		return fmt.Errorf("module generation changed immutable service snapshot files %v", changed)
	}
	return nil
}

func writeCanonicalInventory(path string, inventory *Inventory) error {
	data, err := canonicalInventory(inventory)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write render inventory: %w", err)
	}
	return nil
}

func canonicalInventory(inventory *Inventory) ([]byte, error) {
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode render inventory: %w", err)
	}
	return append(data, '\n'), nil
}

func inventoryUnitNames(units []InventoryUnit) []string {
	names := make([]string, 0, len(units))
	for _, unit := range units {
		if unit.Path != "" {
			names = append(names, unit.Name)
		}
	}
	sort.Strings(names)
	return names
}

// inventoryUnitDirectories returns the distinct, sorted render subdirectories
// holding the inventory's rendered units. It replaces the hardcoded "services"
// directory throughout the publication and observation paths so a new unit kind
// (registered in unitDirectory) flows through staging, pruning, and the
// immutability check without further edits.
func inventoryUnitDirectories(inventory *Inventory) ([]string, error) {
	seen := make(map[string]struct{})
	for _, unit := range inventory.Units {
		if unit.Path == "" {
			continue
		}
		directory, ok := unitDirectory(unit.Kind)
		if !ok {
			return nil, fmt.Errorf("inventory unit %s has unknown kind %q", unit.Name, unit.Kind)
		}
		seen[directory] = struct{}{}
	}
	directories := make([]string, 0, len(seen))
	for directory := range seen {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return directories, nil
}

func existingServiceSnapshot(
	ctx context.Context,
	repo,
	module,
	environment,
	bootstrapRoot string,
) (string, error) {
	remoteRef := "refs/remotes/origin/" + serviceSnapshotBranch(module, environment)
	revision, err := gitCommand(ctx, repo, "for-each-ref", "--format=%(objectname)", remoteRef)
	if err != nil {
		return "", err
	}
	if revision != "" {
		return gitCommand(ctx, repo, "rev-parse", revision+"^{commit}")
	}
	return bootstrapRevision(bootstrapRoot)
}

func snapshotLineageMissing(ctx context.Context, repo, snapshot string) (bool, error) {
	if _, err := gitCommand(ctx, repo, "merge-base", "--is-ancestor", snapshot, "HEAD"); err == nil {
		return false, nil
	}
	if _, err := gitCommand(ctx, repo, "cat-file", "-e", snapshot+"^{commit}"); err != nil {
		return false, fmt.Errorf("resolve previous service snapshot %s: %w", snapshot, err)
	}
	return true, nil
}

func commitServiceSnapshot(ctx context.Context, repo, module, environment, previousSnapshot string) (string, error) {
	head, err := gitCommand(ctx, repo, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	parents := []string{head}
	if previousSnapshot != "" {
		missing, lineageErr := snapshotLineageMissing(ctx, repo, previousSnapshot)
		if lineageErr != nil {
			return "", lineageErr
		}
		if missing {
			parents = append(parents, previousSnapshot)
		}
	}
	var timestamp int64
	for _, parent := range parents {
		rawTimestamp, showErr := gitCommand(ctx, repo, "show", "-s", "--format=%ct", parent)
		if showErr != nil {
			return "", showErr
		}
		parentTimestamp, parseErr := strconv.ParseInt(rawTimestamp, 10, 64)
		if parseErr != nil {
			return "", fmt.Errorf("parse parent commit timestamp %q: %w", rawTimestamp, parseErr)
		}
		if parentTimestamp > timestamp {
			timestamp = parentTimestamp
		}
	}
	tree, err := gitCommand(ctx, repo, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", fmt.Sprintf("Snapshot %s services for %s", module, environment))
	date := fmt.Sprintf("@%d +0000", timestamp+1)
	revision, err := gitCommandWithEnv(
		ctx,
		repo,
		[]string{
			"GIT_AUTHOR_NAME=Codefly GitOps",
			"GIT_AUTHOR_EMAIL=gitops@codefly.dev",
			"GIT_COMMITTER_NAME=Codefly GitOps",
			"GIT_COMMITTER_EMAIL=gitops@codefly.dev",
			"GIT_AUTHOR_DATE=" + date,
			"GIT_COMMITTER_DATE=" + date,
		},
		args...,
	)
	if err != nil {
		return "", fmt.Errorf("create immutable service snapshot: %w", err)
	}
	if _, err := gitCommand(ctx, repo, "reset", "--soft", revision); err != nil {
		return "", err
	}
	return revision, nil
}

func publishServiceSnapshot(ctx context.Context, repo, module, environment, revision string) error {
	snapshotBranch := serviceSnapshotBranch(module, environment)
	refspec := revision + ":refs/heads/" + snapshotBranch
	if _, err := gitCommand(ctx, repo, "push", "--porcelain", "--", "origin", refspec); err != nil {
		return fmt.Errorf("publish immutable service snapshot without force: %w", err)
	}
	remote, err := gitCommand(ctx, repo, "ls-remote", "--exit-code", "--refs", "origin", "refs/heads/"+snapshotBranch)
	if err != nil {
		return fmt.Errorf("verify immutable service snapshot: %w", err)
	}
	fields := strings.Fields(remote)
	if len(fields) != 2 || fields[0] != revision {
		return fmt.Errorf("service snapshot ref resolved to %q, expected %s", remote, revision)
	}
	return nil
}

func serviceSnapshotBranch(module, environment string) string {
	return "codefly/snapshot-" + sanitizeRef(module) + "-" + sanitizeRef(environment)
}

func removePublicationRemainder(target string, unitDirs []string) error {
	keep := map[string]struct{}{moduleBundleDir: {}}
	for _, directory := range unitDirs {
		keep[directory] = struct{}{}
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, ok := keep[entry.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func bootstrapRevision(root string) (string, error) {
	revision := ""
	err := walkBootstrapApplications(root, func(_, current, _ string) error {
		if revision == "" {
			revision = current
			return nil
		}
		if current != revision {
			return fmt.Errorf("bootstrap Applications use different snapshot revisions %s and %s", revision, current)
		}
		return nil
	})
	return revision, err
}

func validateBootstrapRevision(root, expected string) error {
	return walkBootstrapApplications(root, func(path, revision, _ string) error {
		if revision != expected {
			return fmt.Errorf("bootstrap Application %s targets revision %q, expected service snapshot %s", path, revision, expected)
		}
		return nil
	})
}

func validateBootstrapUnits(root, targetPath string, inventory *Inventory, environment string) error {
	expected := make(map[string]struct{}, len(inventory.Units)+1)
	if inventory.ModulePath != "" {
		path := filepath.ToSlash(filepath.Join(targetPath, inventory.ModulePath, "overlays", environment))
		expected[path] = struct{}{}
	}
	for _, unit := range inventory.Units {
		if unit.Path == "" {
			continue
		}
		path := filepath.ToSlash(filepath.Join(targetPath, unit.Path, "overlays", environment))
		expected[path] = struct{}{}
	}
	err := walkBootstrapApplications(root, func(path, _ string, sourcePath string) error {
		if _, exists := expected[sourcePath]; !exists {
			return fmt.Errorf("bootstrap Application %s targets unit path %q outside the rendered unit graph", path, sourcePath)
		}
		delete(expected, sourcePath)
		return nil
	})
	if err != nil {
		return err
	}
	if len(expected) > 0 {
		missing := make([]string, 0, len(expected))
		for path := range expected {
			missing = append(missing, path)
		}
		sort.Strings(missing)
		return fmt.Errorf("module bootstrap is missing Applications for service paths %v", missing)
	}
	return nil
}

func walkBootstrapApplications(root string, visit func(path, revision, sourcePath string) error) error {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("module bootstrap is not a directory")
	}
	return walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != ".yaml" && extension != ".yml" && extension != jsonExtension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests, _, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		for _, item := range manifests {
			if item.group != argoAPIGroup {
				continue
			}
			switch item.kind {
			case kindApplication:
				spec, _ := item.value["spec"].(map[string]any)
				source, _ := spec["source"].(map[string]any)
				revision, _ := source["targetRevision"].(string)
				sourcePath, _ := source["path"].(string)
				if !gitObjectPattern.MatchString(revision) {
					return fmt.Errorf("bootstrap Application %s has non-immutable target revision %q", item.path, revision)
				}
				if err := visit(item.path, revision, sourcePath); err != nil {
					return err
				}
			case kindApplicationSet:
				if err := visitApplicationSet(item, visit); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// visitApplicationSet reports the ApplicationSet as one bootstrap Application per
// promotable component (module resources + services). Every stamped Application
// shares the immutable snapshot revision pinned in the template source, so the
// bootstrap revision and service-graph coverage checks read the ApplicationSet the
// same way they read the per-service Applications it replaced.
func visitApplicationSet(item manifest, visit func(path, revision, sourcePath string) error) error {
	spec, _ := item.value["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	templateSpec, _ := template["spec"].(map[string]any)
	source, _ := templateSpec["source"].(map[string]any)
	revision, _ := source["targetRevision"].(string)
	if !gitObjectPattern.MatchString(revision) {
		return fmt.Errorf("bootstrap ApplicationSet %s has non-immutable target revision %q", item.path, revision)
	}
	for _, sourcePath := range applicationSetComponentPaths(spec) {
		if err := visit(item.path, revision, sourcePath); err != nil {
			return err
		}
	}
	return nil
}

// applicationSetComponentPaths returns each distinct component overlay the
// ApplicationSet stamps. The overlay list generator is repeated once per tenant
// matrix, so paths are de-duplicated to a single occurrence per overlay.
func applicationSetComponentPaths(spec map[string]any) []string {
	seen := map[string]struct{}{}
	var paths []string
	generators, _ := spec["generators"].([]any)
	for _, raw := range generators {
		generator, _ := raw.(map[string]any)
		matrix, _ := generator["matrix"].(map[string]any)
		inner, _ := matrix["generators"].([]any)
		for _, rawInner := range inner {
			nested, _ := rawInner.(map[string]any)
			list, _ := nested["list"].(map[string]any)
			elements, _ := list["elements"].([]any)
			for _, rawElement := range elements {
				element, _ := rawElement.(map[string]any)
				overlay, ok := element["overlay"].(string)
				if !ok || overlay == "" {
					continue
				}
				if _, dup := seen[overlay]; dup {
					continue
				}
				seen[overlay] = struct{}{}
				paths = append(paths, overlay)
			}
		}
	}
	return paths
}

func validatePublishRequest(workspace *resources.Workspace, request *PublishRequest) error {
	if request == nil || request.Module == "" || request.Environment == "" {
		return fmt.Errorf("module and environment are required")
	}
	if workspace == nil {
		return fmt.Errorf("workspace is required")
	}
	if err := validatePathComponent("module", request.Module); err != nil {
		return err
	}
	if err := validatePathComponent("environment", request.Environment); err != nil {
		return err
	}
	environment, err := orchestration.SelectEnvironment(workspace, request.Environment)
	if err != nil {
		return err
	}
	if request.Local && !environment.IsK3d() {
		return fmt.Errorf("local GitOps qualification requires a k3d environment, got %q", request.Environment)
	}
	return nil
}

func prepareRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest) (*preparedRepository, string, error) {
	if request == nil {
		return nil, "", fmt.Errorf("rollback request is required")
	}
	normalized := *request
	normalized.ToRevision = strings.ToLower(strings.TrimSpace(normalized.ToRevision))
	request = &normalized
	if request.ToRevision == "" {
		return nil, "", fmt.Errorf("rollback target revision is required")
	}
	if !gitObjectPattern.MatchString(request.ToRevision) {
		return nil, "", fmt.Errorf("rollback target must be an exact Git object ID")
	}
	if err := validatePublishRequest(workspace, &request.PublishRequest); err != nil {
		return nil, "", err
	}
	if err := requireReviewedRevision(workspace.Dir(), request.Module, request.Environment, request.ToRevision); err != nil {
		return nil, "", err
	}
	config, _, _, _, err := resolveGitops(workspace, request.Environment, request.Local)
	if err != nil {
		return nil, "", err
	}
	temp, err := os.MkdirTemp("", "codefly-gitops-revision-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(temp)
	if _, err := gitCommand(ctx, temp, "clone", "--quiet", "--no-checkout", "--", config.RepoURL, "repo"); err != nil {
		return nil, "", err
	}
	revision, err := gitCommand(ctx, filepath.Join(temp, "repo"), "rev-parse", request.ToRevision+"^{commit}")
	if err != nil {
		return nil, "", fmt.Errorf("resolve rollback revision: %w", err)
	}
	prepared, err := preparePublish(ctx, workspace, &request.PublishRequest, revision, false)
	if err != nil {
		return nil, "", err
	}
	prepared.plan.ID, err = publishPlanID(&prepared.plan, revision)
	if err != nil {
		prepared.cleanup()
		return nil, "", err
	}
	return prepared, revision, nil
}

func requireReviewedRevision(root, module, environment, revision string) error {
	directory := filepath.Join(root, ".codefly", "gitops", "evidence")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("load reviewed promotion evidence: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != jsonExtension {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return fmt.Errorf("read reviewed promotion evidence: %w", err)
		}
		var evidence Evidence
		if err := json.Unmarshal(data, &evidence); err != nil {
			return fmt.Errorf("decode reviewed promotion evidence %s: %w", entry.Name(), err)
		}
		reviewed := evidence.Review.State == "MERGED" && evidence.Review.ReviewDecision == approvedReviewDecision ||
			evidence.Review.State == "LOCAL_REVIEW_REF" && evidence.Review.ReviewDecision == "LOCAL_QUALIFIED"
		if evidence.SchemaVersion == EvidenceSchemaVersion && evidence.Module == module && evidence.Environment == environment &&
			evidence.Health == healthyStatus && reviewed &&
			evidence.SignedCommit == revision {
			return nil
		}
	}
	return fmt.Errorf("rollback target %s has no reviewed Healthy promotion evidence", revision)
}

func commitAndPublish(ctx context.Context, workspace *resources.Workspace, prepared *preparedRepository, request *PublishRequest) (PublishResult, error) {
	message := strings.TrimSpace(request.CommitMessage)
	if message == "" {
		message = fmt.Sprintf("Promote %s to %s", request.Module, request.Environment)
	}
	commit := prepared.plan.ExistingCommit
	if len(prepared.plan.Changed) > 0 {
		if _, err := gitCommand(ctx, prepared.dir, "commit", "--allow-empty", "-S", "-m", message); err != nil {
			return PublishResult{}, fmt.Errorf("create signed promotion commit: %w", err)
		}
		var err error
		commit, err = gitCommand(ctx, prepared.dir, "rev-parse", "HEAD^{commit}")
		if err != nil {
			return PublishResult{}, err
		}
	}
	tree, err := gitCommand(ctx, prepared.dir, "rev-parse", commit+"^{tree}")
	if err != nil {
		return PublishResult{}, err
	}
	rawCommit, err := gitCommand(ctx, prepared.dir, "cat-file", "-p", commit)
	if err != nil {
		return PublishResult{}, err
	}
	if !strings.Contains(rawCommit, "\ngpgsig ") {
		return PublishResult{}, fmt.Errorf("promotion commit %s is not signed", commit)
	}
	if len(prepared.plan.Changed) > 0 {
		refspec := "refs/heads/" + prepared.plan.PromotionBranch + ":refs/heads/" + prepared.plan.PromotionBranch
		if _, err := gitCommand(ctx, prepared.dir, "push", "--porcelain", "--set-upstream", "--", "origin", refspec); err != nil {
			return PublishResult{}, fmt.Errorf("push promotion branch without force: %w", err)
		}
	}
	remote, err := gitCommand(ctx, prepared.dir, "ls-remote", "--exit-code", "--refs", "origin", "refs/heads/"+prepared.plan.PromotionBranch)
	if err != nil {
		return PublishResult{}, fmt.Errorf("verify promotion branch: %w", err)
	}
	fields := strings.Fields(remote)
	if len(fields) < 2 || fields[0] != commit {
		return PublishResult{}, fmt.Errorf("promotion branch resolved to %q, expected %s", remote, commit)
	}
	prURL, prID, err := openOrUpdatePullRequest(ctx, prepared, request, commit)
	if err != nil {
		return PublishResult{}, err
	}
	result := PublishResult{
		PlanID: prepared.plan.ID, Repository: prepared.plan.Repository, Path: prepared.plan.Path,
		BaseBranch: prepared.plan.BaseBranch, PromotionBranch: prepared.plan.PromotionBranch,
		RenderDigest: prepared.plan.RenderDigest, SnapshotRevision: prepared.plan.SnapshotRevision,
		Commit: commit, Tree: tree, Signed: true,
		PullRequest: prURL, PullRequestID: prID,
	}
	if err := writeReceipt(workspace.Dir(), "publications", request.Module+"-"+request.Environment+jsonExtension, result); err != nil {
		return PublishResult{}, err
	}
	return result, nil
}

func clonePromotionRepository(ctx context.Context, repository, baseBranch, promotionBranch string) (string, func(), string, string, error) {
	temp, err := os.MkdirTemp("", "codefly-gitops-publish-")
	if err != nil {
		return "", nil, "", "", fmt.Errorf("create publication checkout: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temp) }
	repo := filepath.Join(temp, "repo")
	if _, err := gitCommand(ctx, temp, "clone", "--quiet", "--no-checkout", "--", repository, repo); err != nil {
		cleanup()
		return "", nil, "", "", err
	}
	if _, err := gitCommand(ctx, repo, "check-ref-format", "--branch", baseBranch); err != nil {
		cleanup()
		return "", nil, "", "", fmt.Errorf("invalid configured GitOps branch %q: %w", baseBranch, err)
	}
	baseRef := "refs/remotes/origin/" + baseBranch
	baseRevision, err := gitCommand(ctx, repo, "rev-parse", baseRef+"^{commit}")
	if err != nil {
		cleanup()
		return "", nil, "", "", fmt.Errorf("resolve configured GitOps branch %q: %w", baseBranch, err)
	}
	if _, err := gitCommand(ctx, repo, "check-ref-format", "--branch", promotionBranch); err != nil {
		cleanup()
		return "", nil, "", "", fmt.Errorf("invalid promotion branch %q: %w", promotionBranch, err)
	}
	remoteRef := "refs/remotes/origin/" + promotionBranch
	branchRevision, branchErr := gitCommand(ctx, repo, "rev-parse", "--verify", remoteRef+"^{commit}")
	if branchErr == nil {
		if _, err := gitCommand(ctx, repo, "checkout", "--quiet", "-b", promotionBranch, remoteRef); err != nil {
			cleanup()
			return "", nil, "", "", err
		}
	} else {
		branchRevision = ""
		if _, err := gitCommand(ctx, repo, "checkout", "--quiet", "-b", promotionBranch, baseRef); err != nil {
			cleanup()
			return "", nil, "", "", err
		}
	}
	status, err := gitCommand(ctx, repo, "status", "--porcelain=v1")
	if err != nil {
		cleanup()
		return "", nil, "", "", err
	}
	if status != "" {
		cleanup()
		return "", nil, "", "", fmt.Errorf("publication checkout is unexpectedly dirty")
	}
	return repo, cleanup, baseRevision, branchRevision, nil
}

func restoreCloneTree(ctx context.Context, repo, targetPath, revision string) error {
	if _, err := gitCommand(ctx, repo, "rm", "-r", "--ignore-unmatch", "--", targetPath); err != nil {
		return err
	}
	if _, err := gitCommand(ctx, repo, "checkout", revision, "--", targetPath); err != nil {
		return fmt.Errorf("restore GitOps tree from %s: %w", revision, err)
	}
	if _, err := confinedJoin(repo, targetPath); err != nil {
		return err
	}
	return nil
}

func replaceCloneTree(source, root, destinationPath string) error {
	destination, err := confinedJoin(root, destinationPath)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return copyTree(source, destination)
}

func stagedPathsSince(ctx context.Context, repo, revision string, paths ...string) ([]string, error) {
	args := make([]string, 0, 6+len(paths))
	args = append(args, "diff", "--cached", "--name-only", "-z", revision, "--")
	args = append(args, paths...)
	output, err := gitCommandBytes(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) > 0 {
			changed = append(changed, string(raw))
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func changedPathsBetween(ctx context.Context, repo, baseRevision, branchRevision string) ([]string, error) {
	output, err := gitCommandBytes(ctx, repo, "diff", "--name-only", "-z", baseRevision+"..."+branchRevision)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) > 0 {
			paths = append(paths, string(raw))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func openOrUpdatePullRequest(ctx context.Context, prepared *preparedRepository, request *PublishRequest, commit string) (string, int, error) {
	if prepared.plan.RepositorySlug == "" {
		reviewRef := localReviewRef(prepared.plan.PromotionBranch, commit)
		refspec := commit + ":" + reviewRef
		if _, err := gitCommand(ctx, prepared.dir, "push", "--porcelain", "--", "origin", refspec); err != nil {
			return "", 0, fmt.Errorf("publish local review ref: %w", err)
		}
		return prepared.plan.Repository + "#" + reviewRef, 0, nil
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = fmt.Sprintf("Promote %s to %s", request.Module, request.Environment)
	}
	body := strings.TrimSpace(request.Body)
	if body == "" {
		body = fmt.Sprintf(
			"Render digest: `%s`\n\nService snapshot: `%s`\n\nSigned commit: `%s`",
			prepared.plan.RenderDigest,
			prepared.plan.SnapshotRevision,
			commit,
		)
	}
	output, err := command(ctx, "", "gh", "pr", "list",
		"--repo", prepared.plan.RepositorySlug, "--head", prepared.plan.PromotionBranch,
		"--base", prepared.plan.BaseBranch, "--state", "open", "--json", "number,url,headRefOid")
	if err != nil {
		return "", 0, fmt.Errorf("inspect promotion pull request: %w", err)
	}
	var existing []struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		HeadRefOID string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(output), &existing); err != nil {
		return "", 0, fmt.Errorf("decode promotion pull request: %w", err)
	}
	if len(existing) > 1 {
		return "", 0, fmt.Errorf("multiple open promotion pull requests target %s", prepared.plan.PromotionBranch)
	}
	if len(existing) == 1 {
		pr := existing[0]
		if pr.HeadRefOID != commit {
			return "", 0, fmt.Errorf("pull request head is %s, expected %s", pr.HeadRefOID, commit)
		}
		if _, err := command(ctx, "", "gh", "pr", "edit", strconv.Itoa(pr.Number),
			"--repo", prepared.plan.RepositorySlug, "--title", title, "--body", body); err != nil {
			return "", 0, fmt.Errorf("update promotion pull request: %w", err)
		}
		return pr.URL, pr.Number, nil
	}
	url, err := command(ctx, "", "gh", "pr", "create", "--repo", prepared.plan.RepositorySlug,
		"--base", prepared.plan.BaseBranch, "--head", prepared.plan.PromotionBranch,
		"--title", title, "--body", body)
	if err != nil {
		return "", 0, fmt.Errorf("open promotion pull request: %w", err)
	}
	return verifyPullRequest(ctx, prepared.plan.RepositorySlug, strings.TrimSpace(url), prepared.plan.BaseBranch, commit)
}

func localReviewRef(promotionBranch, commit string) string {
	return "refs/codefly/reviews/" + strings.ReplaceAll(promotionBranch, "/", "-") + "/" + commit
}

func verifyPullRequest(ctx context.Context, repository, pullRequest, baseBranch, commit string) (string, int, error) {
	output, err := command(ctx, "", "gh", "pr", "view", pullRequest, "--repo", repository,
		"--json", "number,url,headRefOid,baseRefName")
	if err != nil {
		return "", 0, fmt.Errorf("verify promotion pull request: %w", err)
	}
	var response struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		HeadRefOID  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return "", 0, fmt.Errorf("decode verified promotion pull request: %w", err)
	}
	if response.HeadRefOID != commit || response.BaseRefName != baseBranch {
		return "", 0, fmt.Errorf(
			"promotion pull request targets %s at %s, expected %s at %s",
			response.BaseRefName, response.HeadRefOID, baseBranch, commit,
		)
	}
	return response.URL, response.Number, nil
}

func resolveGitops(workspace *resources.Workspace, environment string, local bool) (*repositoryConfig, string, string, string, error) {
	if workspace == nil {
		return nil, "", "", "", fmt.Errorf("workspace is required")
	}
	var config repositoryConfig
	for _, candidate := range workspace.Environments {
		if candidate.Name != environment || candidate.Gitops == nil {
			continue
		}
		config = repositoryConfig{
			RepoURL:      candidate.Gitops.RepoURL,
			FetchRepoURL: candidate.Gitops.FetchRepoURL,
			Path:         candidate.Gitops.Path,
			Branch:       candidate.Gitops.Branch,
		}
		break
	}
	if config.RepoURL == "" && workspace.Gitops != nil {
		config = repositoryConfig{
			RepoURL:      workspace.Gitops.RepoURL,
			FetchRepoURL: workspaceFetchRepository(workspace),
			Path:         workspace.Gitops.Path,
			Branch:       workspace.Gitops.Branch,
		}
	}
	if config.RepoURL == "" {
		return nil, "", "", "", fmt.Errorf("environment %q gitops.repo-url is required", environment)
	}
	slug, err := validateRepositoryURL(config.RepoURL, local)
	if err != nil {
		return nil, "", "", "", err
	}
	baseBranch := strings.TrimSpace(config.Branch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	pathRoot, err := validateRelativePath(config.Path)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("gitops.path: %w", err)
	}
	return &config, slug, baseBranch, pathRoot, nil
}

// localFetchRemoteHost returns the container-DNS host of the managed local k3d
// fetch remote (#201) when the publication promotes locally against a portable,
// non-file repo-url. Argo then fetches from that in-cluster host while repo-url
// stays a committable github URL. It is empty for every other publication,
// leaving the publication-repo match in force.
func localFetchRemoteHost(workspace *resources.Workspace, request *PublishRequest, config *repositoryConfig) (string, error) {
	if !request.Local || strings.TrimSpace(config.FetchRepoURL) == "" {
		return "", nil
	}
	if strings.HasPrefix(strings.TrimSpace(config.RepoURL), "file://") {
		return "", nil
	}
	remote, err := NewFetchRemote(workspace, request.Environment)
	if err != nil {
		return "", fmt.Errorf("derive local fetch remote identity: %w", err)
	}
	return remote.Spec.DNSName, nil
}

func workspaceFetchRepository(workspace *resources.Workspace) string {
	data, err := os.ReadFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName))
	if err != nil {
		return ""
	}
	var document struct {
		Gitops struct {
			FetchRepoURL string `yaml:"fetch-repo-url"`
		} `yaml:"gitops"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return ""
	}
	return document.Gitops.FetchRepoURL
}

func validateRepositoryURL(raw string, local bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("workspace.gitops.repo-url is required")
	}
	if match := scpGitURLPattern.FindStringSubmatch(raw); len(match) == 3 {
		return match[1] + "/" + strings.TrimSuffix(match[2], ".git"), nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("workspace.gitops.repo-url: %w", err)
	}
	if parsed.User != nil && parsed.Scheme == httpsScheme {
		return "", fmt.Errorf("workspace.gitops.repo-url must not contain credentials")
	}
	if parsed.Scheme == sshScheme && parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return "", fmt.Errorf("workspace.gitops.repo-url must not contain credentials")
		}
	}
	if strings.Contains(parsed.Hostname(), "*") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("workspace.gitops.repo-url contains unsafe authority")
	}
	switch parsed.Scheme {
	case httpsScheme, sshScheme:
		if parsed.Hostname() != "github.com" {
			return "", fmt.Errorf("GitHub repository host must be github.com")
		}
		if parsed.Scheme == sshScheme && parsed.User != nil && parsed.User.Username() != "git" {
			return "", fmt.Errorf("GitHub SSH repository user must be git")
		}
		parts := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
		if len(parts) != 2 || !githubSegmentPattern.MatchString(parts[0]) || !githubSegmentPattern.MatchString(parts[1]) {
			return "", fmt.Errorf("workspace.gitops.repo-url must identify owner/repository")
		}
		return parts[0] + "/" + parts[1], nil
	case "file":
		if !local {
			return "", fmt.Errorf("file GitOps repositories are allowed only for local qualification")
		}
		if parsed.User != nil || parsed.Host != "" || !filepath.IsAbs(parsed.Path) {
			return "", fmt.Errorf("local GitOps repository must be an absolute file URL without credentials")
		}
		return "", nil
	default:
		return "", fmt.Errorf("workspace.gitops.repo-url must use HTTPS or SSH")
	}
}

func validateRelativePath(value string) (string, error) {
	if value == "" || value == "." {
		return "", nil
	}
	if strings.Contains(value, `\`) || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%q contains unsafe path characters", value)
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes the repository", value)
	}
	return filepath.ToSlash(clean), nil
}

func confinedJoin(root, relative string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("GitOps destination %q escapes repository", relative)
	}
	current := root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect GitOps destination %q: %w", relative, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("GitOps destination %q traverses symbolic link %s", relative, current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("GitOps destination %q traverses non-directory %s", relative, current)
		}
	}
	return target, nil
}

func publishPlanID(plan *PublishPlan, restoreRevision string) (string, error) {
	planCopy := *plan
	planCopy.ID = ""
	planCopy.Diff = ""
	payload := struct {
		Plan            PublishPlan `json:"plan"`
		DiffSHA256      string      `json:"diffSha256"`
		RestoreRevision string      `json:"restoreRevision,omitempty"`
	}{
		Plan: planCopy, DiffSHA256: hashString(plan.Diff), RestoreRevision: restoreRevision,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode publication plan: %w", err)
	}
	return hashBytes(data), nil
}

func sanitizeRef(value string) string {
	var result strings.Builder
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			result.WriteRune(r)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeReceipt(root, kind, name string, value any) error {
	if filepath.Base(kind) != kind || filepath.Base(name) != name || kind == "." || name == "." {
		return fmt.Errorf("invalid GitOps receipt path")
	}
	dir := filepath.Join(root, ".codefly", "gitops", kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create GitOps receipt directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode GitOps receipt: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return fmt.Errorf("create GitOps receipt: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("install GitOps receipt: %w", err)
	}
	return nil
}

func validatePathComponent(label, value string) error {
	if len(value) > 253 || !pathComponentPattern.MatchString(value) || filepath.Base(value) != value {
		return fmt.Errorf("%s %q is not a safe path component", label, value)
	}
	return nil
}

func LoadPublishResult(root, module, environment string) (PublishResult, error) {
	if err := validatePathComponent("module", module); err != nil {
		return PublishResult{}, err
	}
	if err := validatePathComponent("environment", environment); err != nil {
		return PublishResult{}, err
	}
	path := filepath.Join(root, ".codefly", "gitops", "publications", module+"-"+environment+jsonExtension)
	data, err := os.ReadFile(path)
	if err != nil {
		return PublishResult{}, fmt.Errorf("read publication receipt: %w", err)
	}
	var result PublishResult
	if err := json.Unmarshal(data, &result); err != nil {
		return PublishResult{}, fmt.Errorf("decode publication receipt: %w", err)
	}
	if result.SnapshotRevision == "" || result.Commit == "" || result.Tree == "" || result.RenderDigest == "" || !result.Signed {
		return PublishResult{}, fmt.Errorf("publication receipt is incomplete")
	}
	return result, nil
}

func gitCommand(ctx context.Context, dir string, args ...string) (string, error) {
	return command(ctx, dir, "git", args...)
}

func gitCommandWithEnv(ctx context.Context, dir string, environment []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), environment...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitCommandBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return stdout.Bytes(), nil
}

func command(ctx context.Context, dir, name string, args ...string) (string, error) {
	return commandWithEnvironment(ctx, dir, nil, name, args...)
}

func commandWithEnvironment(
	ctx context.Context,
	dir string,
	environment []string,
	name string,
	args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = environment
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), message)
	}
	return strings.TrimSpace(stdout.String()), nil
}
