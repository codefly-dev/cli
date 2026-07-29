package gitops

import (
	"context"

	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// ProduceRequest names the module (or a single service) and the environment
// whose manifest bundle a ManifestProducer must render.
//
// It deliberately carries no repository, branch, revision, pull-request, Git,
// GitHub, or Argo CD fields: those are promotion concerns and are accepted only
// at this layer, never by the plugins a producer drives.
type ProduceRequest struct {
	Workspace   *resources.Workspace
	Module      *resources.Module
	Service     *resources.Service // nil renders the whole module
	Environment *resources.Environment
	AppProject  string
	// StandAlone renders a single service without its dependencies. It applies
	// only when Service is set and is ignored for a whole-module render.
	StandAlone bool
	Sink       orchestration.OutputSink
}

// ManifestProducer renders a validated, transport-neutral manifest bundle.
//
// Implementations invoke the module's plugins and stop at bundle production;
// they know nothing about how the bundle is later published or reconciled. This
// seam lets tests drive the whole promotion lifecycle from a fake producer, and
// lets the coordinator accept a bundle from any conforming plugin without
// plugin-specific GitOps logic.
type ManifestProducer interface {
	Produce(ctx context.Context, request ProduceRequest) (RenderResult, error)
}

// RepositoryPublisher clones the promotion repository, inventories the reviewed
// tree, and records an immutable, signed, revision-bound promotion. Publication
// is gated by a prepared mutation-authority permit.
type RepositoryPublisher interface {
	Plan(ctx context.Context, workspace *resources.Workspace, request *PublishRequest) (PublishPlan, error)
	Publish(ctx context.Context, workspace *resources.Workspace, mutation *PublishMutation, permit mutationauthority.PreparedPermit) (PublishResult, error)
	PlanRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest) (RollbackPlan, error)
	Rollback(ctx context.Context, workspace *resources.Workspace, mutation *RollbackMutation, permit mutationauthority.PreparedPermit) (PublishResult, error)
}

// ReconciliationObserver verifies Argo CD reconciled the exact reviewed revision
// with a matching source, project, cluster identity, and health, then records
// auditable evidence.
type ReconciliationObserver interface {
	Observe(ctx context.Context, request *ObserveRequest) (ObserveResult, error)
}

// Coordinator composes the promotion adapters behind one in-process API that the
// command-line frontend and a future Codefly server/controller drive
// identically. The repository, branch, revision, review-provider, and Argo
// identities enter here; the producer never sees them.
type Coordinator struct {
	Producer  ManifestProducer
	Publisher RepositoryPublisher
	Observer  ReconciliationObserver
}

// NewCoordinator wires the production adapters backed by this package.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		Producer:  flowProducer{},
		Publisher: repositoryPublisher{},
		Observer:  argoObserver{},
	}
}

// Render produces the manifest bundle for a module or single service.
func (c *Coordinator) Render(ctx context.Context, request ProduceRequest) (RenderResult, error) {
	return c.Producer.Produce(ctx, request)
}

// PlanPublish inspects the exact publication diff for a rendered bundle.
func (c *Coordinator) PlanPublish(ctx context.Context, workspace *resources.Workspace, request *PublishRequest) (PublishPlan, error) {
	return c.Publisher.Plan(ctx, workspace, request)
}

// Publish records a signed, revision-bound promotion and opens or updates its
// review.
func (c *Coordinator) Publish(ctx context.Context, workspace *resources.Workspace, mutation *PublishMutation, permit mutationauthority.PreparedPermit) (PublishResult, error) {
	return c.Publisher.Publish(ctx, workspace, mutation, permit)
}

// PlanRollback inspects the re-promotion diff for a prior reviewed revision.
func (c *Coordinator) PlanRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest) (RollbackPlan, error) {
	return c.Publisher.PlanRollback(ctx, workspace, request)
}

// Rollback re-promotes a prior reviewed tree through a new signed review.
func (c *Coordinator) Rollback(ctx context.Context, workspace *resources.Workspace, mutation *RollbackMutation, permit mutationauthority.PreparedPermit) (PublishResult, error) {
	return c.Publisher.Rollback(ctx, workspace, mutation, permit)
}

// Observe verifies Argo CD reconciliation and stores evidence.
func (c *Coordinator) Observe(ctx context.Context, request *ObserveRequest) (ObserveResult, error) {
	return c.Observer.Observe(ctx, request)
}

type flowProducer struct{}

func (flowProducer) Produce(ctx context.Context, request ProduceRequest) (RenderResult, error) {
	if request.Service == nil {
		return RenderModule(ctx, request.Workspace, request.Module, request.Environment, request.AppProject, request.Sink)
	}
	return RenderService(ctx, request.Workspace, request.Module, request.Service, request.Environment, request.AppProject, request.StandAlone, request.Sink)
}

type repositoryPublisher struct{}

func (repositoryPublisher) Plan(ctx context.Context, workspace *resources.Workspace, request *PublishRequest) (PublishPlan, error) {
	return PlanPublish(ctx, workspace, request)
}

func (repositoryPublisher) Publish(ctx context.Context, workspace *resources.Workspace, mutation *PublishMutation, permit mutationauthority.PreparedPermit) (PublishResult, error) {
	return Publish(ctx, workspace, mutation, permit)
}

func (repositoryPublisher) PlanRollback(ctx context.Context, workspace *resources.Workspace, request *RollbackRequest) (RollbackPlan, error) {
	return PlanRollback(ctx, workspace, request)
}

func (repositoryPublisher) Rollback(ctx context.Context, workspace *resources.Workspace, mutation *RollbackMutation, permit mutationauthority.PreparedPermit) (PublishResult, error) {
	return Rollback(ctx, workspace, mutation, permit)
}

type argoObserver struct{}

func (argoObserver) Observe(ctx context.Context, request *ObserveRequest) (ObserveResult, error) {
	return Observe(ctx, request)
}
