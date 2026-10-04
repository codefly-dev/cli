package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v89/github"
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
	mutation.Request.Carriers = mutation.Carriers
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
	if violation := firstContractViolation(prepared.plan.ContractChecks); violation != "" {
		return PublishResult{}, fmt.Errorf("gitops publish blocked by contract admission: %s", violation)
	}
	return commitAndPublish(ctx, workspace, prepared, &mutation.Request)
}

func PlanRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest) (RollbackPlan, error) {
	prepared, revision, err := prepareRollback(ctx, workspace, request, false)
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
	mutation.Request.Carriers = mutation.Carriers
	inspected, _, err := prepareRollback(ctx, workspace, &mutation.Request, false)
	if err != nil {
		return PublishResult{}, err
	}
	if inspected.plan.ID != mutation.PlanID {
		current := inspected.plan.ID
		inspected.cleanup()
		return PublishResult{}, fmt.Errorf("rollback plan is stale: prepared %s, current %s", mutation.PlanID, current)
	}
	inspected.cleanup()
	// The rollback's snapshot — the restored workloads with their re-settled
	// documents — is advertised as a publish's is, so the Applications can
	// reach the revision they are stamped with.
	prepared, _, err := prepareRollback(ctx, workspace, &mutation.Request, true)
	if err != nil {
		return PublishResult{}, err
	}
	defer prepared.cleanup()
	if prepared.plan.ID != mutation.PlanID {
		return PublishResult{}, fmt.Errorf("rollback plan changed while advertising its snapshot: prepared %s, current %s", mutation.PlanID, prepared.plan.ID)
	}
	if violation := firstContractViolation(prepared.plan.ContractChecks); violation != "" {
		return PublishResult{}, fmt.Errorf("gitops rollback blocked by contract admission: %s", violation)
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
	if err := rejectUnboundPublication(ctx, workspace, request.Module); err != nil {
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
	if err = refuseSharedDeliveryPath(workspace, request.Environment, request.Local); err != nil {
		return nil, err
	}
	publication, err := newDeliveryPublication(request, baseBranch)
	if err != nil {
		return nil, err
	}
	// The environment's host block shapes delivery on every publish, a rollback
	// included: what a rollback re-delivers is settled against the base branch
	// and signed now, exactly as a render is, never restored as the bytes an
	// earlier publish signed.
	env, err := orchestration.SelectEnvironment(workspace, request.Environment)
	if err != nil {
		return nil, err
	}
	publication.workspace, publication.env = workspace, env
	if err = publication.addressHost(ctx, workspace, env); err != nil {
		return nil, err
	}
	rendered := filepath.Join(workspace.Dir(), "deployments", "modules", request.Module)
	var inventory Inventory
	if restoreRevision == "" {
		if inventory, err = loadRenderedPublication(ctx, workspace, request, env, rendered, pathRoot, publication); err != nil {
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
		if err = refuseUnrelatedPromotionChanges(ctx, repo, baseRevision, branchRevision, promotionBranch, targetPath, publication.cellPath); err != nil {
			return fail(err)
		}
	}
	startRevision := baseRevision
	if branchRevision != "" {
		startRevision = branchRevision
	}
	clone := &publicationClone{
		repo: repo, target: target, targetPath: targetPath, config: config,
		publishSnapshot: publishSnapshot, localFetchHost: localFetchHost, publication: publication,
	}
	var snapshotRevision string
	if restoreRevision == "" {
		snapshotRevision, inventory, err = stageRenderedPublication(ctx, workspace, request, clone, rendered, &inventory)
	} else {
		snapshotRevision, inventory, err = stageRollbackPublication(ctx, workspace, request, env, clone, restoreRevision)
	}
	if err != nil {
		return fail(err)
	}
	publishedPaths := []string{targetPath}
	if publication.cellPath != "" {
		publishedPaths = append(publishedPaths, publication.cellPath)
	}
	if _, addErr := gitCommand(ctx, repo, append([]string{gitAddVerb, "-A", "--"}, publishedPaths...)...); addErr != nil {
		return fail(addErr)
	}
	contractChecks := checkContracts(&inventory, resolveGitopsModuleInventory(ctx, repo, baseBranch, pathRoot), request.AllowUnresolvedContracts)
	staleOnBase, err := staleBaseConsumers(ctx, repo, baseBranch, pathRoot, request.Module, inventory.WorkspaceConfigurationDigests)
	if err != nil {
		return fail(err)
	}
	if err = refuseStaleBaseConsumers(workspace.Dir(), request.Environment, baseBranch, staleOnBase); err != nil {
		return fail(err)
	}
	staleConsumers := describeStaleConsumers(staleOnBase)
	changed, err := stagedPathsSince(ctx, repo, startRevision, publishedPaths...)
	if err != nil {
		return fail(err)
	}
	if len(changed) == 0 {
		if branchRevision == "" {
			return fail(fmt.Errorf("promotion has no changes"))
		}
	}
	diff, err := gitCommand(ctx, repo, append([]string{"diff", "--cached", "--binary", startRevision, "--"}, publishedPaths...)...)
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
		Changed: changed, Diff: diff, ContractChecks: contractChecks,
		Delivery:       inventory.Delivery,
		StaleConsumers: staleConsumers,
		Carriers:       publication.options.Carriers,
	}
	plan.ID, err = publishPlanID(&plan, restoreRevision)
	if err != nil {
		return fail(err)
	}
	return &preparedRepository{
		dir: repo, cleanup: cleanup, plan: plan,
	}, nil
}

// newDeliveryPublication shapes how a publish signs and checks what it
// delivers. A local qualification publish never signs — not "signs when it
// can": run from a workflow that holds an OIDC identity, it would otherwise
// deliver signed carriers and a Job to a qualification cluster under the
// release identity. The signer is the one a process with no identity gets,
// unless the caller supplied one. A release publish signs under the
// workflow's identity and checks each carrier the way a host will: a carrier
// a host would refuse is refused here, in front of whoever ran the release.
func newDeliveryPublication(request *PublishRequest, baseBranch string) (*deliveryPublication, error) {
	publication := &deliveryPublication{baseBranch: baseBranch, options: deliveryPublishOptions{
		Signer: request.Signer, AllowUnsigned: request.Local, Module: request.Module,
	}}
	if request.Local && request.Signer == nil {
		publication.options.Signer = &signing.Unavailable{Reason: "a local qualification publish delivers its documents unsigned"}
	}
	// The carriers an inspected plan signed: the publish executing it
	// delivers those exact bytes, so the plan it compares against is the one
	// it was given — signing is once, in the plan, and reuse is verified.
	publication.options.Reuse = request.Carriers
	publication.options.Executing = request.Carriers != nil
	publication.options.Resign = request.Resign
	return publication, nil
}

// requireReleaseIdentity: an environment that declares a host is delivered
// signed carriers, so a release publish to it needs the identity it signs and
// checks under — the workflow's, as GitHub Actions states it to the job. The
// checks are built from that environment and are nil wherever it is absent,
// local or not; without them an unchanged render could reuse delivered
// carriers under no verification policy at all and a fresh signature would
// go unchecked, so a hosted release publish with no identity refuses before
// it reads a document. A local qualification publish never signs, and a
// caller-supplied signer is the test seam that brings its own checks.
func (p *deliveryPublication) requireReleaseIdentity() error {
	if p.options.AllowUnsigned || p.options.Signer != nil || p.options.SelfCheck != nil && p.options.ReuseCheck != nil {
		return nil
	}
	return errors.New("the environment declares a host, so this publish delivers signed carriers; a release publish signs and checks them under the workflow identity it runs with, and this process has none (GITHUB_REPOSITORY and GITHUB_WORKFLOW_REF are not both set): run it from the release workflow, or publish --local to a qualification cluster")
}

// holdToReleasePolicy builds, for a release publish, the two checks its
// carriers are held to from the policy the composition states beside the
// host block: the fresh-signature check is this workflow's exact identity,
// admitted by the policy first, and the reuse check is the policy itself. A
// hosted environment that states no policy cannot be released to, since
// nothing reviewed would say which identity the host accepts.
func (p *deliveryPublication) holdToReleasePolicy(env *environments.Environment) error {
	if p.options.AllowUnsigned || p.options.Signer != nil {
		return nil
	}
	if env.Host.Release == nil {
		return fmt.Errorf("environment %s declares a host but no release policy; a release publish signs under the identity the host accepts, which the composition states as host.release (repository, workflow, refs)", env.Name)
	}
	policy := &signing.ReleasePolicy{Repository: env.Host.Release.Repository, Workflow: env.Host.Release.Workflow, Refs: env.Host.Release.Refs}
	selfCheck, err := signing.SelfCheckUnder(nil, policy)
	if err != nil {
		return err
	}
	reuseCheck, err := signing.ReleaseCheckUnder(policy)
	if err != nil {
		return err
	}
	p.options.SelfCheck, p.options.ReuseCheck = selfCheck, reuseCheck
	return nil
}

// addressHost points the publication at the environment's host: the delivery
// API its Jobs post to, and the envelope revision and the domain its
// documents must declare.
func (p *deliveryPublication) addressHost(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) error {
	// Hosted delivery is what needs the release identity; a publication with
	// no host delivers nothing signed and needs none. (An environment that
	// dropped its host while documents are rendered for it is refused later,
	// by refuseUnaddressedDocuments, not admitted here.)
	if env.Host != nil {
		if err := p.holdToReleasePolicy(env); err != nil {
			return err
		}
		if err := p.requireReleaseIdentity(); err != nil {
			return err
		}
	}
	target, err := resolveDeliveryTarget(ctx, workspace, env)
	if err != nil {
		return err
	}
	p.options.Target = target
	if env.Host != nil {
		p.options.EnvelopeRevision = env.Host.EnvelopeRevision
		p.options.Domain = env.Host.Domain
		p.options.Coordinate = env.Host.Coordinate
		p.options.Component = env.Host.Component
		p.options.TrustDomain = env.Host.TrustDomain
		p.options.Audience = env.Host.Audience
	}
	return nil
}

// loadRenderedPublication reads the tree the render left in the workspace and
// holds the groups it bakes in to the composition as it is now, and to the
// sibling consumers rendered beside it: each refusal is lifted by one render,
// named. A cell file rendered for the environment is staged with the tree.
func loadRenderedPublication(
	ctx context.Context,
	workspace *resources.Workspace,
	request *PublishRequest,
	env *environments.Environment,
	rendered, pathRoot string,
	publication *deliveryPublication,
) (Inventory, error) {
	inventory, err := loadPublicationInventory(ctx, workspace, request, rendered, pathRoot)
	if err != nil {
		return Inventory{}, err
	}
	if err = holdRenderedGroups(ctx, workspace, request, env, &inventory); err != nil {
		return Inventory{}, err
	}
	// The cell is the platform's inventory of what this publish delivers:
	// where the environment declares a host, a render without it is not
	// publishable, and a cell that cannot be read is an error, never an
	// absent one.
	cell := cellPath(workspace.Dir(), request.Environment)
	_, statErr := os.Stat(cell)
	switch {
	case statErr == nil:
		publication.cellSource = cell
		publication.cellPath = filepath.ToSlash(filepath.Join(pathRoot, cellsDir, request.Environment, CellFileName))
	case errors.Is(statErr, fs.ErrNotExist):
		if env.Host != nil {
			return Inventory{}, fmt.Errorf("the environment %s declares a host but no cell file is rendered for it at %s; render %s for %s before publishing", request.Environment, cell, request.Module, request.Environment)
		}
	default:
		return Inventory{}, fmt.Errorf("read the cell file %s: %w", cell, statErr)
	}
	return inventory, nil
}

// isModuleNotFound tells a workspace that holds no module of the name from a
// module that exists and cannot be loaded; core reports the first by message.
func isModuleNotFound(err error) bool {
	return strings.Contains(err.Error(), "cannot find module")
}

// holdRenderedGroups holds a tree about to be published — a render's or a
// rollback's — to the workspace configuration groups as they are now. The
// groups held against are the ones the composition's services and contract
// consume NOW, derived from the composition at publish, not re-read from the
// digests the render recorded, which could not say that a group consumed
// since the render exists at all.
func holdRenderedGroups(ctx context.Context, workspace *resources.Workspace, request *PublishRequest, env *environments.Environment, inventory *Inventory) error {
	var current map[string]string
	var err error
	module, loadErr := workspace.LoadModuleFromName(ctx, request.Module)
	switch {
	case loadErr == nil:
		current, err = currentGroupDigests(ctx, workspace, env, module)
	case isModuleNotFound(loadErr):
		// No sources for this module in the workspace — a packaged solution
		// — so the groups held against are the ones its render recorded,
		// digested as the environment provides them now; a group consumed
		// since that render is not findable here, and the doc says so.
		current, err = digestRecordedGroups(ctx, workspace, env, inventory.WorkspaceConfigurationDigests)
	default:
		return fmt.Errorf("the module %s this publish delivers cannot be loaded from the workspace, so the configuration groups its services consume now are unknown: %w", request.Module, loadErr)
	}
	if err != nil {
		return err
	}
	if err = refuseStaleRender(request.Module, request.Environment, inventory.WorkspaceConfigurationDigests, current); err != nil {
		return err
	}
	return refuseStaleGroupConsumers(workspace.Dir(), request.Module, request.Environment, inventory.WorkspaceConfigurationDigests)
}

// refuseSharedDeliveryPath refuses to publish an environment whose delivery
// path — repository, branch and path — another environment of the workspace
// delivers to as well, when either declares a host: a module tree there is
// replaced whole by whichever environment publishes last, so the other's
// delivered generations and tombstones would become absence, and a replay
// would have no record of what was withdrawn. Each hosted environment needs a
// path of its own.
func refuseSharedDeliveryPath(workspace *resources.Workspace, environment string, local bool) error {
	self, err := environments.Select(workspace, environment)
	if err != nil {
		return err
	}
	config, _, branch, root, err := resolveGitops(workspace, environment, local)
	if err != nil {
		return err
	}
	for _, resource := range workspace.Environments {
		if resource == nil || resource.Name == environment {
			continue
		}
		other, selectErr := environments.Select(workspace, resource.Name)
		if selectErr != nil {
			return selectErr
		}
		if self.Host == nil && other.Host == nil {
			continue
		}
		otherConfig, _, otherBranch, otherRoot, resolveErr := resolveGitops(workspace, resource.Name, local)
		if resolveErr != nil {
			// An environment that delivers nowhere shares nothing.
			continue
		}
		if otherConfig.RepoURL == config.RepoURL && otherBranch == branch && otherRoot == root {
			return fmt.Errorf("environments %s and %s both deliver to %s on branch %s at %s; an environment that declares a host needs a delivery path of its own, or one publish would erase the other's delivered generations and tombstones", environment, resource.Name, config.RepoURL, branch, root)
		}
	}
	return nil
}

// refuseUnrelatedPromotionChanges holds an existing promotion branch to the
// module's own path and its cell: anything else on it is someone else's work,
// which this publish must neither carry nor overwrite.
func refuseUnrelatedPromotionChanges(ctx context.Context, repo, baseRevision, branchRevision, promotionBranch, targetPath, cell string) error {
	existing, err := changedPathsBetween(ctx, repo, baseRevision, branchRevision)
	if err != nil {
		return err
	}
	for _, changed := range existing {
		if changed != targetPath && !strings.HasPrefix(changed, targetPath+"/") && changed != cell {
			return fmt.Errorf("promotion branch %s contains unrelated change %s", promotionBranch, changed)
		}
	}
	return nil
}

// publicationClone is the promotion clone a publication is staged into, with
// what snapshotting a tree there needs.
type publicationClone struct {
	repo, target, targetPath string
	config                   *repositoryConfig
	publishSnapshot          bool
	localFetchHost           string
	publication              *deliveryPublication
}

// stageRenderedPublication publishes the tree the render left in the
// workspace: snapshotted, settled and signed, with its cell contribution
// staged beside it.
func stageRenderedPublication(
	ctx context.Context,
	workspace *resources.Workspace,
	request *PublishRequest,
	clone *publicationClone,
	rendered string,
	inventory *Inventory,
) (string, Inventory, error) {
	generateBootstrap, err := publicationGeneratesBootstrap(ctx, workspace, request.Module, inventory)
	if err != nil {
		return "", Inventory{}, err
	}
	snapshotRevision, published, err := prepareServicePublication(
		ctx, clone.repo, clone.target, clone.targetPath, rendered, inventory, generateBootstrap,
		request.Environment, clone.config, clone.publishSnapshot, clone.localFetchHost, clone.publication,
	)
	if err != nil {
		return "", Inventory{}, err
	}
	if err = stageCellFile(ctx, clone.repo, clone.publication, rendered, &published, true); err != nil {
		return "", Inventory{}, err
	}
	return snapshotRevision, published, nil
}

// stageRollbackPublication publishes the tree an earlier revision delivered:
// restored into the clone, then snapshotted, settled and signed exactly as a
// render would be. Restoring the old bootstrap and the old carriers verbatim
// would point every Application at a snapshot holding the carriers that
// revision signed, which a host past them refuses.
func stageRollbackPublication(
	ctx context.Context,
	workspace *resources.Workspace,
	request *PublishRequest,
	env *environments.Environment,
	clone *publicationClone,
	restoreRevision string,
) (string, Inventory, error) {
	if err := restoreCloneTree(ctx, clone.repo, clone.targetPath, restoreRevision); err != nil {
		return "", Inventory{}, err
	}
	restored, err := os.MkdirTemp("", "codefly-rollback-")
	if err != nil {
		return "", Inventory{}, err
	}
	defer os.RemoveAll(restored)
	if err = copyTree(clone.target, restored); err != nil {
		return "", Inventory{}, fmt.Errorf("copy the restored tree: %w", err)
	}
	// The restore put the historical revision in place whole, every
	// environment's delivery overlay included; what the base branch delivered
	// comes back before anything is settled, so another environment's
	// tombstone is never replaced by that revision's live bytes. This
	// environment's own historical overlay is in the copy above, and is
	// staged and settled from it as a render's would be.
	if err = restoreDeliveredOverlays(ctx, clone.repo, clone.publication.baseBranch, clone.targetPath); err != nil {
		return "", Inventory{}, err
	}
	if err = ValidateRenderedTree(restored, "", true); err != nil {
		return "", Inventory{}, fmt.Errorf("validate rollback render: %w", err)
	}
	inventory, err := LoadInventory(restored)
	if err != nil {
		return "", Inventory{}, err
	}
	// A restored tree is held to the groups as they are now exactly as a
	// render is: a rollback that would resurrect configuration the
	// composition no longer provides is refused, by name, until re-rendered.
	if err = holdRenderedGroups(ctx, workspace, request, env, &inventory); err != nil {
		return "", Inventory{}, err
	}
	generateBootstrap, err := publicationGeneratesBootstrap(ctx, workspace, request.Module, &inventory)
	if err != nil {
		return "", Inventory{}, err
	}
	snapshotRevision, published, err := prepareServicePublication(
		ctx, clone.repo, clone.target, clone.targetPath, restored, &inventory, generateBootstrap,
		request.Environment, clone.config, clone.publishSnapshot, clone.localFetchHost, clone.publication,
	)
	if err != nil {
		return "", Inventory{}, err
	}
	// The cell describes what is published: a rollback contributes the cell
	// of the tree it restores, derived from that tree, so the platform's
	// inventory follows the workloads back.
	if err = stageCellFile(ctx, clone.repo, clone.publication, restored, &published, false); err != nil {
		return "", Inventory{}, err
	}
	return snapshotRevision, published, nil
}

// resolveGitopsModuleInventory resolves a consumed contract's exposing module
// against its rendered inventory at
// <gitopsPath>/deployments/modules/<module>/.codefly-render.json, read from
// the base branch (refs/remotes/origin/baseBranch) rather than repo's checked-
// out working tree. The working tree is the promotion branch, which is
// long-lived and reused across every publish for the module owning it
// (clonePromotionRepository checks it out as-is when it already exists); only
// the one module path actively being republished ever gets refreshed from
// history, so any *other* module's working-tree copy can be arbitrarily stale
// relative to main. Reading the git blob directly from the base branch is the
// only way to see the exposing module's true, currently-reviewed state.
//
// module is untrusted — it comes from contract provenance that ultimately
// traces back to a library a service depends on — so it is validated as a
// safe path component before being used to build the blob's tree path; unlike
// a filesystem path, a git tree pathspec has no ".." escape to confine, but
// the same validation keeps the error message meaningful and rejects
// injection of ":" or "/" that could otherwise be mistaken for ref syntax.
func resolveGitopsModuleInventory(ctx context.Context, repo, baseBranch, gitopsPath string) func(module string) (*Inventory, error) {
	return func(module string) (*Inventory, error) {
		if err := validatePathComponent("consumed contract module", module); err != nil {
			return nil, err
		}
		relative := filepath.ToSlash(filepath.Join(gitopsPath, "deployments", "modules", module, InventoryFilename))
		blob := "refs/remotes/origin/" + baseBranch + ":" + relative
		if _, err := gitCommand(ctx, repo, "cat-file", "-e", blob); err != nil {
			return nil, fmt.Errorf("module %s has no inventory on %s: %w", module, baseBranch, os.ErrNotExist)
		}
		data, err := gitCommandBytes(ctx, repo, "show", blob)
		if err != nil {
			return nil, err
		}
		inventory, err := decodeInventory(data, "render")
		if err != nil {
			return nil, err
		}
		return &inventory, nil
	}
}

// checkContracts admits every consumed contract carried by the consumer
// inventory's units against the exposing module's inventory, resolved through
// resolve. It is pure and side-effect free: resolve is the only way it reaches
// outside the consumer inventory, which is what lets tests exercise it against
// hand-built inventories without a real GitOps checkout. allowUnresolved
// downgrades a consumed contract to skipped only when the exposing module
// genuinely has no inventory yet (resolve's error satisfies
// errors.Is(err, os.ErrNotExist)) — every other resolve failure (an unsafe
// module name, a corrupt or non-canonical host inventory, an unsupported
// schema) stays a hard violation and carries resolve's real error text,
// regardless of the flag.
func checkContracts(consumer *Inventory, resolve func(module string) (*Inventory, error), allowUnresolved bool) []ContractCheck {
	var checks []ContractCheck
	for i := range consumer.Units {
		unit := &consumer.Units[i]
		for j := range unit.Contracts {
			contract := &unit.Contracts[j]
			if contract.Role != ContractRoleConsumes {
				continue
			}
			checks = append(checks, checkContract(unit.Name, contract, resolve, allowUnresolved))
		}
	}
	return checks
}

func checkContract(unitName string, contract *InventoryContract, resolve func(string) (*Inventory, error), allowUnresolved bool) ContractCheck {
	check := ContractCheck{Unit: unitName, Module: contract.Module, Service: contract.Service, Endpoint: contract.Endpoint}
	host, err := resolve(contract.Module)
	if err == nil && host == nil {
		err = fmt.Errorf("resolver returned no inventory for module %s", contract.Module)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if allowUnresolved {
				check.Status = ContractCheckSkipped
				check.Message = fmt.Sprintf(
					"consumes %s/%s/%s: module %s is not deployed yet in the target GitOps tree (allowed by --allow-unresolved-contracts)",
					contract.Module, contract.Service, contract.Endpoint, contract.Module,
				)
				return check
			}
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf(
				"consumes %s/%s/%s but module %s is not deployed in the target GitOps tree",
				contract.Module, contract.Service, contract.Endpoint, contract.Module,
			)
			return check
		}
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"consumes %s/%s/%s but module %s's inventory could not be resolved: %v",
			contract.Module, contract.Service, contract.Endpoint, contract.Module, err,
		)
		return check
	}
	var exposed *InventoryContract
	for _, hostUnit := range host.Units {
		if hostUnit.Name != contract.Service {
			continue
		}
		for i := range hostUnit.Contracts {
			candidate := hostUnit.Contracts[i]
			if candidate.Role == ContractRoleExposes && candidate.Endpoint == contract.Endpoint {
				exposed = &candidate
				break
			}
		}
	}
	if exposed == nil {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf("module %s no longer exposes %s/%s", contract.Module, contract.Service, contract.Endpoint)
		return check
	}
	if exposed.Package != contract.Package {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s now exposes %s/%s as proto package %s, not the consumed package %s",
			contract.Module, contract.Service, contract.Endpoint, exposed.Package, contract.Package,
		)
		return check
	}
	if host.Package == nil {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s has no package version recorded and does not satisfy consumed contract %s/%s",
			contract.Module, contract.Service, contract.Endpoint,
		)
		return check
	}
	compatibleNewerHost := false
	if contract.Constraint != "" {
		constraint, err := semver.NewConstraint(contract.Constraint)
		if err != nil {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf("consumed constraint %q is invalid: %v", contract.Constraint, err)
			return check
		}
		hostVersion, err := semver.NewVersion(host.Package.Version)
		if err != nil {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf("module %s package version %q is invalid", contract.Module, host.Package.Version)
			return check
		}
		if !constraint.Check(hostVersion) {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf(
				"module %s package %s does not satisfy consumed constraint %s",
				contract.Module, host.Package.Version, contract.Constraint,
			)
			return check
		}
		pinnedVersion, err := semver.NewVersion(contract.Version)
		if err != nil {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf("consumed pinned version %q is invalid", contract.Version)
			return check
		}
		// Only a host that is semantically newer than the version the client was
		// generated from is "drift" (a compatible upgrade the client hasn't caught
		// up to yet). A host that is the same version or older with a different
		// digest is inconsistent data, not a benign upgrade, and stays a violation.
		compatibleNewerHost = hostVersion.GreaterThan(pinnedVersion)
	} else if host.Package.Version != contract.Version {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s package %s does not satisfy pinned version %s",
			contract.Module, host.Package.Version, contract.Version,
		)
		return check
	}
	if exposed.Digest != contract.Digest {
		if compatibleNewerHost {
			check.Status = ContractCheckDrift
			check.Message = fmt.Sprintf(
				"module %s package %s satisfies %s but the client was generated from an older contract",
				contract.Module, host.Package.Version, contract.Constraint,
			)
			return check
		}
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s contract digest %s does not match consumed digest %s",
			contract.Module, exposed.Digest, contract.Digest,
		)
		return check
	}
	check.Status = ContractCheckOK
	return check
}

// firstContractViolation returns the message of the first unresolved contract
// violation, or "" when every check passed, was skipped, or drifted onto a
// compatible newer host.
func firstContractViolation(checks []ContractCheck) string {
	for _, check := range checks {
		if check.Status == ContractCheckViolation {
			return check.Message
		}
	}
	return ""
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
	if inventoryHasSolutionUnits(&inventory) {
		if err := validateSolutionUnits(request.Module, inventory.Units); err != nil {
			return Inventory{}, err
		}
		return inventory, nil
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

// inventoryHasSolutionUnits reports whether a publication carries solution units.
// A solution owns its own unit graph and Argo transport, so it is published
// without a service-module resource to cross-check against.
func inventoryHasSolutionUnits(inventory *Inventory) bool {
	for _, unit := range inventory.Units {
		if unit.Kind == UnitKindSolution {
			return true
		}
	}
	return false
}

// validateSolutionUnits checks a solution publication's unit graph. A solution's
// units are defined by the executor's render, not by a workspace module, so the
// only invariants to enforce are that every rendered unit is a solution unit of
// the published module — no service units smuggled in, no foreign module.
func validateSolutionUnits(moduleName string, units []InventoryUnit) error {
	for _, unit := range units {
		if unit.Kind != UnitKindSolution {
			return fmt.Errorf("solution publication contains non-solution unit %q of kind %q", unit.Name, unit.Kind)
		}
		if unit.Module != moduleName {
			return fmt.Errorf("rendered solution unit %q belongs to module %q, expected %q", unit.Name, unit.Module, moduleName)
		}
	}
	return nil
}

// publicationGeneratesBootstrap reports whether the CLI owns Argo bootstrap
// generation for this publication. A solution renders its transport through the
// CLI exactly like an agent-backed module; a plain service module without an
// agent instead ships a pre-rendered bootstrap in its own tree.
func publicationGeneratesBootstrap(ctx context.Context, workspace *resources.Workspace, moduleName string, inventory *Inventory) (bool, error) {
	if inventoryHasSolutionUnits(inventory) {
		return true, nil
	}
	module, err := workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return false, fmt.Errorf("load rendered module %q: %w", moduleName, err)
	}
	return module.Agent != nil, nil
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
		_, expectedManaged := selectedEnvironment.ManagedService(moduleName, unit.Name)
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
	generateBootstrap bool,
	environment string,
	config *repositoryConfig,
	publishSnapshot bool,
	localFetchHost string,
	publication *deliveryPublication,
) (string, Inventory, error) {
	snapshot, err := prepareServiceSnapshot(
		ctx,
		repo,
		target,
		targetPath,
		rendered,
		renderedInventory,
		environment,
		publishSnapshot,
		publication,
	)
	if err != nil {
		return "", Inventory{}, err
	}
	delivery := snapshot.delivery
	if generateBootstrap {
		if err = generateArgoBootstrap(
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
			if err = copyTree(renderedBootstrap, filepath.Join(target, "bootstrap")); err != nil {
				return "", Inventory{}, fmt.Errorf("stage rendered bootstrap: %w", err)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", Inventory{}, fmt.Errorf("inspect rendered bootstrap: %w", statErr)
		}
	}
	if err = verifyServiceSnapshotBinding(ctx, repo, snapshot.revision, snapshot.servicePaths); err != nil {
		return "", Inventory{}, err
	}
	if err = validateBootstrapRevision(filepath.Join(target, "bootstrap"), snapshot.revision); err != nil {
		return "", Inventory{}, err
	}
	if generateBootstrap {
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
		Module:                        renderedInventory.Module,
		UnitNames:                     snapshot.services,
		OwnedPath:                     targetPath,
		ModulePath:                    renderedInventory.ModulePath,
		Units:                         renderedInventory.Units,
		Package:                       renderedInventory.Package,
		Environment:                   renderedInventory.Environment,
		Namespace:                     renderedInventory.Namespace,
		AppProject:                    renderedInventory.AppProject,
		Promotable:                    true,
		CheckUnitDirectories:          true,
		SolutionHostBindingPath:       renderedInventory.SolutionHostBindingPath,
		HostsDelivery:                 renderedInventory.HostsDelivery,
		SolutionAuthorityPath:         renderedInventory.SolutionAuthorityPath,
		Delivered:                     delivery,
		WorkspaceConfigurationDigests: renderedInventory.WorkspaceConfigurationDigests,
	}
	if _, err = validateTree(target, options); err != nil {
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
	// delivery is what the snapshot's delivery overlays settled to.
	delivery *InventoryDelivery
}

func prepareServiceSnapshot(
	ctx context.Context,
	repo,
	target,
	targetPath,
	rendered string,
	renderedInventory *Inventory,
	environment string,
	publishSnapshot bool,
	publication *deliveryPublication,
) (serviceSnapshotPreparation, error) {
	moduleName := renderedInventory.Module
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
	existingSnapshot, err := existingServiceSnapshot(ctx, repo, moduleName, environment, filepath.Join(target, "bootstrap"))
	if err != nil {
		return serviceSnapshotPreparation{}, err
	}
	if err = removePublicationRemainder(target, unitDirs); err != nil {
		return serviceSnapshotPreparation{}, err
	}
	// The delivery documents are part of the snapshot: every Application the
	// bootstrap stamps reads the ONE immutable snapshot revision, the delivery
	// Applications included, so the overlays they read must be in it. They
	// were once staged after the snapshot was committed, which left every
	// delivery Application pointing at a revision where its path did not
	// exist. Settled here, against the base branch, before the commit.
	delivery, err := stageAndSettleDelivery(ctx, repo, target, targetPath, rendered, renderedInventory, environment, publication)
	if err != nil {
		return serviceSnapshotPreparation{}, err
	}
	serviceNames := inventoryUnitNames(renderedInventory.Units)
	snapshotOptions := &RenderOptions{
		Module:                  renderedInventory.Module,
		UnitNames:               serviceNames,
		OwnedPath:               targetPath,
		ModulePath:              renderedInventory.ModulePath,
		Units:                   renderedInventory.Units,
		Package:                 renderedInventory.Package,
		Environment:             renderedInventory.Environment,
		Namespace:               renderedInventory.Namespace,
		AppProject:              renderedInventory.AppProject,
		Promotable:              true,
		SolutionHostBindingPath: renderedInventory.SolutionHostBindingPath,
		HostsDelivery:           renderedInventory.HostsDelivery,
		SolutionAuthorityPath:   renderedInventory.SolutionAuthorityPath,
		Delivered:               delivery,
	}
	snapshotInventory, err := buildInventory(target, snapshotOptions)
	if err != nil {
		return serviceSnapshotPreparation{}, err
	}
	if err = writeCanonicalInventory(filepath.Join(target, InventoryFilename), &snapshotInventory); err != nil {
		return serviceSnapshotPreparation{}, fmt.Errorf("write service snapshot inventory: %w", err)
	}
	if err = ValidateServiceSnapshot(target); err != nil {
		return serviceSnapshotPreparation{}, fmt.Errorf("validate service snapshot: %w", err)
	}
	if _, err = gitCommand(ctx, repo, "add", "-A", "--", targetPath); err != nil {
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
		snapshotRevision, err = commitServiceSnapshot(ctx, repo, moduleName, environment, existingSnapshot)
		if err != nil {
			return serviceSnapshotPreparation{}, err
		}
	}
	if publishSnapshot {
		if err := publishServiceSnapshot(ctx, repo, moduleName, environment, snapshotRevision); err != nil {
			return serviceSnapshotPreparation{}, err
		}
	}
	return serviceSnapshotPreparation{
		revision:     snapshotRevision,
		services:     serviceNames,
		servicePaths: servicePaths,
		delivery:     delivery,
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
	// The delivery directories are kept whole: they hold every environment's
	// delivered overlay, and a publish stages its own environment's only.
	keep := map[string]struct{}{moduleBundleDir: {}, solutionHostBindingDir: {}, solutionAuthorityDir: {}}
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
	if inventory.SolutionHostBindingPath != "" {
		expected[filepath.ToSlash(filepath.Join(targetPath, inventory.SolutionHostBindingPath, "overlays", environment))] = struct{}{}
	}
	if inventory.SolutionAuthorityPath != "" {
		expected[filepath.ToSlash(filepath.Join(targetPath, inventory.SolutionAuthorityPath, "overlays", environment))] = struct{}{}
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
	return walkRegularFiles(root, func(_, relative string, _ os.FileInfo) error {
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != yamlExtension && extension != ymlExtension && extension != jsonExtension {
			return nil
		}
		data, err := readWithin(root, relative)
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

func prepareRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest, publishSnapshot bool) (*preparedRepository, string, error) {
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
	if err := rejectUnboundPublication(ctx, workspace, request.Module); err != nil {
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
	if _, err = gitCommand(ctx, temp, "clone", "--quiet", "--no-checkout", "--", config.RepoURL, "repo"); err != nil {
		return nil, "", err
	}
	revision, err := gitCommand(ctx, filepath.Join(temp, "repo"), "rev-parse", request.ToRevision+"^{commit}")
	if err != nil {
		return nil, "", fmt.Errorf("resolve rollback revision: %w", err)
	}
	prepared, err := preparePublish(ctx, workspace, &request.PublishRequest, revision, publishSnapshot)
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
		if _, err = gitCommand(ctx, prepared.dir, "push", "--porcelain", "--set-upstream", "--", "origin", refspec); err != nil {
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
		ContractChecks: prepared.plan.ContractChecks,
		Delivery:       prepared.plan.Delivery,
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
	if _, err = gitCommand(ctx, temp, "clone", "--quiet", "--no-checkout", "--", repository, repo); err != nil {
		cleanup()
		return "", nil, "", "", err
	}
	if _, err = gitCommand(ctx, repo, "check-ref-format", "--branch", baseBranch); err != nil {
		cleanup()
		return "", nil, "", "", fmt.Errorf("invalid configured GitOps branch %q: %w", baseBranch, err)
	}
	baseRef := "refs/remotes/origin/" + baseBranch
	baseRevision, err := gitCommand(ctx, repo, "rev-parse", baseRef+"^{commit}")
	if err != nil {
		cleanup()
		return "", nil, "", "", fmt.Errorf("resolve configured GitOps branch %q: %w", baseBranch, err)
	}
	if _, err = gitCommand(ctx, repo, "check-ref-format", "--branch", promotionBranch); err != nil {
		cleanup()
		return "", nil, "", "", fmt.Errorf("invalid promotion branch %q: %w", promotionBranch, err)
	}
	remoteRef := "refs/remotes/origin/" + promotionBranch
	branchRevision, branchErr := gitCommand(ctx, repo, "rev-parse", "--verify", remoteRef+"^{commit}")
	if branchErr == nil {
		if _, err = gitCommand(ctx, repo, "checkout", "--quiet", "-b", promotionBranch, remoteRef); err != nil {
			cleanup()
			return "", nil, "", "", err
		}
	} else {
		branchRevision = ""
		if _, err = gitCommand(ctx, repo, "checkout", "--quiet", "-b", promotionBranch, baseRef); err != nil {
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

// restoreDeliveredOverlays puts the delivery directories of a module path back
// as the base branch delivered them, after a restore put a historical revision
// of them in place: a rollback is computed against delivered history exactly
// as a forward publish is. A directory the base branch does not hold is left
// absent.
func restoreDeliveredOverlays(ctx context.Context, repo, baseBranch, targetPath string) error {
	ref := "refs/remotes/origin/" + baseBranch
	for _, directory := range []string{solutionHostBindingDir, solutionAuthorityDir} {
		path := filepath.ToSlash(filepath.Join(targetPath, directory))
		// The restore left the historical overlays staged; -f drops them
		// whether staged or not.
		if _, err := gitCommand(ctx, repo, "rm", "-r", "-f", "-q", "--ignore-unmatch", "--", path); err != nil {
			return err
		}
		if _, err := gitCommand(ctx, repo, "checkout", ref, "--", path); err != nil {
			if gitSaysAbsent(err) {
				continue
			}
			return fmt.Errorf("restore the delivered %s from %s: %w", directory, baseBranch, err)
		}
	}
	return nil
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
	owner, repo, err := splitRepositorySlug(prepared.plan.RepositorySlug)
	if err != nil {
		return "", 0, err
	}
	client, err := gh.NewClient()
	if err != nil {
		return "", 0, err
	}
	existing, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open",
		Head:  owner + ":" + prepared.plan.PromotionBranch,
		Base:  prepared.plan.BaseBranch,
	})
	if err != nil {
		return "", 0, fmt.Errorf("inspect promotion pull request: %w", err)
	}
	if len(existing) > 1 {
		return "", 0, fmt.Errorf("multiple open promotion pull requests target %s", prepared.plan.PromotionBranch)
	}
	if len(existing) == 1 {
		pr := existing[0]
		if pr.GetHead().GetSHA() != commit {
			return "", 0, fmt.Errorf("pull request head is %s, expected %s", pr.GetHead().GetSHA(), commit)
		}
		if _, _, editErr := client.PullRequests.Edit(ctx, owner, repo, pr.GetNumber(), &github.PullRequest{
			Title: github.Ptr(title),
			Body:  github.Ptr(body),
		}); editErr != nil {
			return "", 0, fmt.Errorf("update promotion pull request: %w", editErr)
		}
		return pr.GetHTMLURL(), pr.GetNumber(), nil
	}
	created, _, err := client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.Ptr(title),
		Head:  github.Ptr(prepared.plan.PromotionBranch),
		Base:  github.Ptr(prepared.plan.BaseBranch),
		Body:  github.Ptr(body),
	})
	if err != nil {
		return "", 0, fmt.Errorf("open promotion pull request: %w", err)
	}
	return verifyPullRequest(ctx, client, owner, repo, created.GetNumber(), prepared.plan.BaseBranch, commit)
}

func splitRepositorySlug(slug string) (string, string, error) {
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("invalid repository %q", slug)
	}
	return owner, repo, nil
}

func localReviewRef(promotionBranch, commit string) string {
	return "refs/codefly/reviews/" + strings.ReplaceAll(promotionBranch, "/", "-") + "/" + commit
}

func verifyPullRequest(ctx context.Context, client *github.Client, owner, repo string, number int, baseBranch, commit string) (string, int, error) {
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", 0, fmt.Errorf("verify promotion pull request: %w", err)
	}
	if pr.GetHead().GetSHA() != commit || pr.GetBase().GetRef() != baseBranch {
		return "", 0, fmt.Errorf(
			"promotion pull request targets %s at %s, expected %s at %s",
			pr.GetBase().GetRef(), pr.GetHead().GetSHA(), baseBranch, commit,
		)
	}
	return pr.GetHTMLURL(), pr.GetNumber(), nil
}

func resolveGitops(workspace *resources.Workspace, environment string, local bool) (*repositoryConfig, string, string, string, error) {
	if workspace == nil {
		return nil, "", "", "", fmt.Errorf("workspace is required")
	}
	var config repositoryConfig
	declared, err := environments.FromWorkspace(workspace)
	if err != nil {
		return nil, "", "", "", err
	}
	for _, candidate := range declared {
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
	defaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		return nil, "", "", "", err
	}
	if config.RepoURL == "" && defaults != nil {
		// An environment may override the path alone; the render lays its
		// tree out under it (moduleOwnedPath), so the publish delivers there
		// — in the workspace's repository, on its branch.
		path := config.Path
		config = repositoryConfig{
			RepoURL:      defaults.RepoURL,
			FetchRepoURL: defaults.FetchRepoURL,
			Path:         defaults.Path,
			Branch:       defaults.Branch,
		}
		if path != "" {
			config.Path = path
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
	// The carriers are the plan's output, carried to the publish that
	// executes it; the plan is identified by what they sign, not by them.
	planCopy.Carriers = nil
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

// deliveryPublication is what settling the delivery documents at publish needs
// beyond the staged tree: the base branch they are settled against, the signer
// and target, and the cell file to carry into the repository.
type deliveryPublication struct {
	baseBranch string
	options    deliveryPublishOptions
	// workspace and env are what deriving the cell contribution from a staged
	// tree needs: the services the tree's units render, and the host block.
	workspace *resources.Workspace
	env       *environments.Environment
	// cellSource is the environment's cell file under the workspace, and
	// cellPath where it lands in the repository, outside every module path.
	// Both empty when the workspace has rendered no cell for this environment.
	cellSource string
	cellPath   string
}

// stageAndSettleDelivery copies the render's delivery documents into the
// staged tree and settles them against the base branch.
func stageAndSettleDelivery(
	ctx context.Context,
	repo, target, targetPath, rendered string,
	inventory *Inventory,
	environment string,
	publication *deliveryPublication,
) (*InventoryDelivery, error) {
	if publication == nil {
		return nil, nil
	}
	// Only this environment's overlay is staged, into a delivery directory
	// that keeps every other environment's as delivered: a publish never
	// removes a document it did not author under its own environment, so a
	// path another environment delivered to — one no longer in the
	// configuration included — keeps its generations and its tombstones. A
	// render that declares none here clears its own overlay for the
	// tombstones the settlement writes from the base branch's record.
	for _, directory := range []string{solutionHostBindingDir, solutionAuthorityDir} {
		overlay := filepath.ToSlash(filepath.Join(directory, "overlays", environment))
		source := filepath.Join(rendered, filepath.FromSlash(overlay))
		switch _, err := os.Stat(source); {
		case err == nil:
			if err = replaceCloneTree(source, repo, filepath.ToSlash(filepath.Join(targetPath, overlay))); err != nil {
				return nil, fmt.Errorf("stage rendered delivery documents: %w", err)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err = os.RemoveAll(filepath.Join(target, filepath.FromSlash(overlay))); err != nil {
				return nil, fmt.Errorf("clear the delivery overlay the render no longer declares: %w", err)
			}
		default:
			return nil, fmt.Errorf("read the rendered delivery overlay: %w", err)
		}
	}
	presence, err := settlePresenceDelivery(ctx, repo, publication.baseBranch, target, targetPath, environment, inventory, &publication.options)
	if err != nil {
		return nil, err
	}
	authority, err := settleAuthorityDelivery(ctx, repo, publication.baseBranch, target, targetPath, environment, inventory, presence, &publication.options)
	if err != nil {
		return nil, err
	}
	return mergeDeliveries(presence, authority), nil
}

// mergeDeliveries joins what the two settlements delivered into one inventory
// record: signed only when both halves are.
func mergeDeliveries(parts ...*InventoryDelivery) *InventoryDelivery {
	var merged *InventoryDelivery
	for _, part := range parts {
		if part == nil {
			continue
		}
		if merged == nil {
			merged = &InventoryDelivery{Signed: true}
		}
		merged.Signed = merged.Signed && part.Signed
		if merged.Identity == "" {
			merged.Identity = part.Identity
		}
		merged.Documents = append(merged.Documents, part.Documents...)
	}
	return merged
}

// stageCellFile stages this module's contribution to the environment's cell
// file: the namespace entry its render produced, merged into the cell the base
// branch already holds, every other module's entry kept as delivered. The local
// cell is built from whatever module trees are on disk, so copying it whole
// let the last module published decide the platform's inventory for the whole
// cell — a CI job rendering one module erased the others from policy input
// while their workloads stayed deployed. A module is removed from the cell by
// withdrawing it, never by another module's publish.
// stageCellFile stages the publishing module's cell contribution, DERIVED from
// the exact tree being published — its workloads, images, selectors, endpoints
// and release as the staged manifests and inventory carry them — never read
// from a file that could describe another tree. A forward publish also holds
// the workspace's cell file (the render's output) to that derivation and
// refuses a contribution that does not describe the rendered tree; a rollback
// has no render to hold, and contributes the restored tree's cell.
func stageCellFile(ctx context.Context, repo string, publication *deliveryPublication, tree string, inventory *Inventory, rendered bool) error {
	if publication == nil || publication.cellPath == "" || publication.env == nil || publication.env.Host == nil {
		return nil
	}
	contribution, err := deriveCellContribution(ctx, publication.workspace, publication.env, tree, inventory)
	if err != nil {
		return err
	}
	if rendered && publication.cellSource != "" {
		if err := refuseStaleCellFile(publication.cellSource, contribution, publication.options.Module); err != nil {
			return err
		}
	}
	return stageCellContribution(ctx, repo, publication, contribution, inventory)
}

// deriveCellContribution is the render's own cell derivation applied to one
// tree: the module's namespace entry, under the host the environment declares.
func deriveCellContribution(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, tree string, inventory *Inventory) (*CellFile, error) {
	consumers, err := endpointConsumers(ctx, workspace)
	if err != nil {
		return nil, err
	}
	namespace, err := cellNamespace(ctx, workspace, env, tree, inventory, consumers)
	if err != nil {
		return nil, fmt.Errorf("derive the cell contribution of module %s from the tree it publishes: %w", inventory.Module, err)
	}
	cell := &CellFile{Schema: CellSchemaV1, Environment: env.Name, Namespaces: []CellNamespace{namespace}}
	if env.Host != nil {
		cell.Coordinate, cell.Component, cell.Domain, cell.TrustDomain = env.Host.Coordinate, env.Host.Component, env.Host.Domain, env.Host.TrustDomain
	}
	return cell, nil
}

// refuseStaleCellFile holds the workspace's cell file — the render's output,
// which a later render, an edit or a failed cell generation can leave behind
// — to the contribution derived from the tree being published.
func refuseStaleCellFile(source string, derived *CellFile, module string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read the cell file: %w", err)
	}
	var local CellFile
	if err = yaml.Unmarshal(data, &local); err != nil {
		return fmt.Errorf("decode the cell file: %w", err)
	}
	var recorded *CellNamespace
	for index := range local.Namespaces {
		if local.Namespaces[index].Module == module {
			recorded = &local.Namespaces[index]
		}
	}
	if recorded == nil {
		return fmt.Errorf("the cell file %s carries no entry for module %s; render again before publishing", source, module)
	}
	have, err := yaml.Marshal(recorded)
	if err != nil {
		return err
	}
	want, err := yaml.Marshal(&derived.Namespaces[0])
	if err != nil {
		return err
	}
	if !bytes.Equal(have, want) {
		return fmt.Errorf("the cell file %s does not describe the rendered tree of module %s (its workloads, images, selectors, endpoints or release differ from what the tree publishes); render again before publishing", source, module)
	}
	return nil
}

// stageCellContribution holds a contribution to the host declared now and
// merges it into the delivered cell.
func stageCellContribution(ctx context.Context, repo string, publication *deliveryPublication, contribution *CellFile, inventory *Inventory) error {
	destination, err := confinedJoin(repo, publication.cellPath)
	if err != nil {
		return err
	}
	if err = refuseCellForAnotherHost(contribution, &publication.options); err != nil {
		return err
	}
	merged, err := mergeCellContribution(ctx, repo, publication.baseBranch, publication.cellPath, contribution, publication.options.Module, consumedEndpoints(inventory))
	if err != nil {
		return err
	}
	var data []byte
	if data, err = yaml.Marshal(merged); err != nil {
		return fmt.Errorf("encode the cell file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create the cell directory: %w", err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return fmt.Errorf("stage the cell file: %w", err)
	}
	return nil
}

// mergeCellContribution takes the publishing module's namespace entry from the
// locally rendered cell and sets it into the cell the base branch delivers,
// keeping every other module's entry. The cell's own fields — schema,
// coordinate, component, domain, trust domain — are the composition's
// declaration and come from the local render. With no cell on the base
// branch, the result holds this module's entry alone: the others join on
// their own publishes.
func mergeCellContribution(ctx context.Context, repo, baseBranch, cellPath string, local *CellFile, module string, consumed []consumedEndpoint) (*CellFile, error) {
	var contribution *CellNamespace
	for index := range local.Namespaces {
		if local.Namespaces[index].Module == module {
			contribution = &local.Namespaces[index]
			break
		}
	}
	if contribution == nil {
		return nil, fmt.Errorf("the rendered cell file carries no entry for module %s; render %s for this environment before publishing it", module, module)
	}
	merged := *local
	merged.Namespaces = nil
	data, showErr := gitCommandBytes(ctx, repo, "show", "refs/remotes/origin/"+baseBranch+":"+cellPath)
	switch {
	case showErr == nil:
		var delivered CellFile
		if decodeErr := yaml.Unmarshal(data, &delivered); decodeErr != nil {
			return nil, fmt.Errorf("the cell file delivered on %s cannot be read, so this module's contribution cannot be merged into it: %w", baseBranch, decodeErr)
		}
		if err := refuseCellHeaderChange(&delivered, local, baseBranch); err != nil {
			return nil, err
		}
		for index := range delivered.Namespaces {
			if delivered.Namespaces[index].Module == module {
				continue
			}
			namespace := delivered.Namespaces[index]
			reconcileConsumers(&namespace, module, consumed)
			merged.Namespaces = append(merged.Namespaces, namespace)
		}
	case gitSaysAbsent(showErr):
		// No cell delivered for this environment yet: this contribution is
		// the first, and the file is this module's entry alone.
	default:
		return nil, fmt.Errorf("the cell file delivered on %s cannot be read, so this module's contribution cannot be merged into it: %w", baseBranch, showErr)
	}
	merged.Namespaces = append(merged.Namespaces, *contribution)
	sort.Slice(merged.Namespaces, func(i, j int) bool { return merged.Namespaces[i].Name < merged.Namespaces[j].Name })
	return &merged, nil
}

// refuseCellForAnotherHost holds the rendered cell to the host the environment
// declares at publish, as every delivered document is: a cell rendered before
// the host block changed is refused by name, never merged under a declaration
// it does not describe.
func refuseCellForAnotherHost(cell *CellFile, opts *deliveryPublishOptions) error {
	if opts.Coordinate == "" {
		return nil
	}
	if cell.Coordinate != opts.Coordinate || cell.Component != opts.Component || cell.Domain != opts.Domain || cell.TrustDomain != opts.TrustDomain {
		return fmt.Errorf("the cell file describes host %s/%s under domain %q (trust domain %q) and the environment declares %s/%s under %q (%q) now; render again before publishing",
			cell.Coordinate, cell.Component, cell.Domain, cell.TrustDomain, opts.Coordinate, opts.Component, opts.Domain, opts.TrustDomain)
	}
	return nil
}

// gitSaysAbsent reports a git read that failed because the path or the ref
// does not exist, as opposed to a repository that could not be read at all.
func gitSaysAbsent(err error) bool {
	message := err.Error()
	for _, absent := range []string{"does not exist in", "exists on disk, but not in", "Not a valid object name", "invalid object name", "did not match any file(s) known to git"} {
		if strings.Contains(message, absent) {
			return true
		}
	}
	return false
}

// refuseCellHeaderChange keeps a cell one host's record: the delivered cell's
// schema, coordinate, component, domain and trust domain are what every other
// module's entry was written under, so a contribution made under another
// declaration is refused rather than relabelling entries it does not own.
func refuseCellHeaderChange(delivered, local *CellFile, baseBranch string) error {
	if delivered.Schema == local.Schema && delivered.Coordinate == local.Coordinate && delivered.Component == local.Component &&
		delivered.Domain == local.Domain && delivered.TrustDomain == local.TrustDomain {
		return nil
	}
	return fmt.Errorf("the cell delivered on %s is the %s record of host %s/%s under domain %q (trust domain %q); this render describes host %s/%s under domain %q (trust domain %q), and a cell is one host's record: withdraw the delivered cell, or render against the declaration it was made under",
		baseBranch, delivered.Schema, delivered.Coordinate, delivered.Component, delivered.Domain, delivered.TrustDomain,
		local.Coordinate, local.Component, local.Domain, local.TrustDomain)
}

// consumedEndpoint is one edge a module's render declares: a unit of the
// module consuming an endpoint of another module's service.
type consumedEndpoint struct {
	Provider, Endpoint, Consumer string
}

// consumedEndpoints lists the edges the render's inventory declares.
func consumedEndpoints(inventory *Inventory) []consumedEndpoint {
	var edges []consumedEndpoint
	for i := range inventory.Units {
		unit := &inventory.Units[i]
		for j := range unit.Contracts {
			contract := &unit.Contracts[j]
			if contract.Role != ContractRoleConsumes {
				continue
			}
			edges = append(edges, consumedEndpoint{
				Provider: resources.ServiceUnique(contract.Module, contract.Service),
				Endpoint: contract.Endpoint,
				Consumer: resources.ServiceUnique(inventory.Module, unit.Name),
			})
		}
	}
	return edges
}

// reconcileConsumers rewrites, in another module's delivered entry, the
// consumer lists that belong to the publishing module: every consumer of that
// module is dropped and the ones its render declares now are added, so an
// edge the module grew or dropped reaches the provider's entry even when the
// provider is not rendered in this workspace.
func reconcileConsumers(namespace *CellNamespace, module string, consumed []consumedEndpoint) {
	prefix := module + "/"
	for w := range namespace.Workloads {
		workload := &namespace.Workloads[w]
		for e := range workload.Endpoints {
			endpoint := &workload.Endpoints[e]
			var kept []string
			for _, consumer := range endpoint.Consumers {
				if !strings.HasPrefix(consumer, prefix) {
					kept = append(kept, consumer)
				}
			}
			for _, edge := range consumed {
				if edge.Provider == workload.Service && edge.Endpoint == endpoint.Name && !slices.Contains(kept, edge.Consumer) {
					kept = append(kept, edge.Consumer)
				}
			}
			sort.Strings(kept)
			endpoint.Consumers = kept
		}
	}
}
