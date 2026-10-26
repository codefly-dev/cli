package gitops

import (
	"time"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/composition"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

const (
	InventoryFilename = ".codefly-render.json"
	SchemaVersion     = 8
	// There is no prior schema a loader still accepts. Schema 8 delivers one
	// module manifest per module (moduleManifestPath) where schema 7 delivered
	// presence, authority and endpoint documents, which no host reads any
	// more: a tree an older CLI rendered is rendered again, never read.
	EvidenceSchemaVersion = 1
)

// ContractRoleExposes and ContractRoleConsumes are the two roles an
// InventoryContract can hold with respect to the unit that carries it.
const (
	ContractRoleExposes  = "exposes"
	ContractRoleConsumes = "consumes"
)

// ContractCheckOK, ContractCheckViolation, ContractCheckSkipped, and
// ContractCheckDrift are the possible ContractCheck.Status values a
// consumed-contract admission check can report.
const (
	ContractCheckOK        = "ok"
	ContractCheckViolation = "violation"
	ContractCheckSkipped   = "skipped"
	ContractCheckDrift     = "drift"
)

// UnitKindService is the artifact kind for a codefly service.
const UnitKindService = "service"

// UnitKindSolution is the artifact kind for a codefly solution: a packaged unit
// that carries its own namespace anatomy (routes, needs, and the services it
// bundles) and renders through the same promotable pipeline as a service.
const UnitKindSolution = "solution"

// moduleBundleDir is the render subdirectory holding the module-level bundle. It
// sits beside the per-unit directories but is not itself a unit, so it is not
// derived from unitDirectory.
const moduleBundleDir = "module"

// serviceUnitDir is the render subdirectory holding service units.
const serviceUnitDir = "services"

// solutionUnitDir is the render subdirectory holding solution units.
const solutionUnitDir = "solutions"

// graphStageDir is the scratch directory, a sibling of the owned tree inside
// the render's staging root, where a module render stages the services of the
// *other* modules its dependency graph pulls in. Those manifests belong to
// their own module's render, so they are kept out of the owned tree entirely
// rather than written beside the module's own units, where a shared service
// name would collide. The staging root is removed when the render finishes.
const graphStageDir = ".graph"

// unitDirectory maps an artifact kind to the render subdirectory that holds its
// units, reporting whether the kind is known. Generalizing the render path
// beyond services is a matter of adding a case here rather than threading a new
// shape through the inventory. The ok return keeps callers from ever joining an
// empty segment for an unrecognized kind.
func unitDirectory(kind string) (string, bool) {
	switch kind {
	case UnitKindService:
		return serviceUnitDir, true
	case UnitKindSolution:
		return solutionUnitDir, true
	default:
		return "", false
	}
}

type Inventory struct {
	SchemaVersion int    `json:"schemaVersion"`
	Module        string `json:"module"`
	Unit          string `json:"unit,omitempty"`
	Environment   string `json:"environment"`
	Namespace     string `json:"namespace,omitempty"`
	AppProject    string `json:"appProject"`
	OwnedPath     string `json:"ownedPath"`
	ModulePath    string `json:"modulePath,omitempty"`
	// ModuleManifestPath is the render subdirectory holding the module's
	// manifest, when this render declared one. Publish points an Argo
	// Application at its environment overlay and derives the promotion's
	// authority from it, exactly as it does for a unit.
	ModuleManifestPath string `json:"moduleManifestPath,omitempty"`
	// HostsDelivery is true for the module that serves the environment's
	// delivery API: its own delivery cannot precede the API it posts to, so
	// it is ordered after the module's units where every other module's
	// delivery goes beside them.
	HostsDelivery bool `json:"hostsDelivery,omitempty"`
	// Package identifies the module package this render was produced from
	// (module.package.codefly.yaml id/version), when the module has one.
	Package *InventoryPackage `json:"package,omitempty"`
	// Pin records how the composition pinned this module (see InventoryPin).
	Pin *InventoryPin `json:"pin,omitempty"`
	// Delivery records the delivery documents publish settled and signed into
	// this tree: each document's identity, generation and whether it is a
	// tombstone, and the identity that signed them. A render records none; the
	// generation is settled, and the signature made, at publish.
	Delivery *InventoryDelivery `json:"delivery,omitempty"`
	// WorkspaceConfigurationDigests records, per workspace configuration group
	// the module's services consume, a digest of the group as the environment
	// provided it to this render. A sibling module consuming the same group
	// and recording another digest was rendered against another value, and the
	// render refuses to deliver two values of one group.
	WorkspaceConfigurationDigests map[string]string `json:"workspaceConfigurationDigests,omitempty"`
	// MeshEdges are the L4 edges into this module's workloads, as the
	// composition declares them: the data the cell's applier compiles into
	// its mesh policy (mesh_edges.go). Absent when the environment declares
	// no host.
	MeshEdges *MeshEdges      `json:"meshEdges,omitempty"`
	Units     []InventoryUnit `json:"units"`
	Files     []InventoryFile `json:"files"`
	Digest    string          `json:"digest"`
	// Dev lists the dev deployments (`codefly deploy dev`) applied on top of
	// this render: services whose image was re-pinned to code no release
	// describes. A full render re-derives the tree and never carries it over.
	Dev []InventoryDevDeployment `json:"dev,omitempty"`
}

type InventoryPackage struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Pin modes, as the inventory spells how the composition pinned a module.
const (
	// PinModeTag: the version is one a tag answers — an exact version, a
	// range or `latest`.
	PinModeTag = "tag"
	// PinModeCommit: the version is a full commit SHA, carried in Commit.
	PinModeCommit = "commit"
)

// InventoryPin records how the composition pinned the rendered module: the
// identity the workspace composes it by, the version it asked for, and
// whether that version is a tag or a commit. It is the fact a cell's loader
// judges a delivery on — a commit pin is for validation, so a non-validation
// cell refuses one — and it is absent for a module the workspace composes
// from its own tree, which pins nothing.
type InventoryPin struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Commit  string `json:"commit,omitempty"`
}

type InventoryUnit struct {
	Kind      string                     `json:"kind"`
	Module    string                     `json:"module"`
	Name      string                     `json:"name"`
	Path      string                     `json:"path,omitempty"`
	Managed   bool                       `json:"managed,omitempty"`
	Bootstrap bool                       `json:"bootstrap,omitempty"`
	Output    *InventoryKubernetesOutput `json:"output,omitempty"`
	// Contracts are the API contracts this unit exposes (from the module's
	// contracts/api catalog) and consumes (from the libraries its service
	// declares in library-dependencies with a sources[] block).
	Contracts []InventoryContract `json:"contracts,omitempty"`
	// Endpoints are the ports the render resolved for each endpoint the unit
	// serves in-cluster — the Service port a consumer connects to and the
	// container port the mesh authorizes — from the flow's network mappings
	// and the unit's rendered pods. Recorded for every pod-producing unit, a
	// managed store included, so the cell file reads them back and keeps no
	// port matcher of its own.
	Endpoints []InventoryEndpoint `json:"endpoints,omitempty"`
}

// InventoryEndpoint is one in-cluster endpoint of a unit with its resolved ports.
type InventoryEndpoint struct {
	Name       string `json:"name"`
	Port       uint32 `json:"port,omitempty"`
	TargetPort uint32 `json:"targetPort,omitempty"`
}

// InventoryContract records one API contract a unit exposes or consumes,
// binding a client-generation-time snapshot (Digest, and for a consumed
// contract, Version/Constraint) to the endpoint it was generated against.
type InventoryContract struct {
	Role     string `json:"role"` // "exposes" | "consumes"
	Module   string `json:"module"`
	Service  string `json:"service"`
	Endpoint string `json:"endpoint"`
	Package  string `json:"package"` // proto package, e.g. saas.accounts.v1
	Digest   string `json:"digest"`  // sha256:… of the contract file
	// Consumes only: the module package version the client was generated from,
	// and the constraint the solution declared (empty = pinned exactly).
	Version    string   `json:"version,omitempty"`
	Constraint string   `json:"constraint,omitempty"`
	Services   []string `json:"services,omitempty"`
}

type InventoryKubernetesOutput struct {
	Kind            string                         `json:"kind"`
	Profile         string                         `json:"profile"`
	ContractVersion string                         `json:"contractVersion"`
	Validation      *InventoryKubernetesValidation `json:"validation"`
}

// InventoryKubernetesValidation is the agent's validation evidence as the
// inventory persists it.
//
// Restricted, not promotable. Core's proto says promotable "names a delivery
// decision in a plugin-facing contract", retained only for migration, while
// restricted "reports a security property, never a delivery decision". This
// type carried only the delivery decision, so the inventory could not
// represent the security property at all: admission asserted `promotable` on
// output whose profile it had just required to be RESTRICTED_PORTABLE_V1, and
// an agent reporting restricted without promotable passed the builder gate and
// then became unacceptable persisted evidence.
//
// The legacy field is deleted rather than kept beside the new one. Two
// booleans for one property is how they came to disagree.
type InventoryKubernetesValidation struct {
	StaticValidation     string   `json:"staticValidation"`
	ServerSideValidation string   `json:"serverSideValidation"`
	Restricted           bool     `json:"restricted"`
	Violations           []string `json:"violations"`
}

type KubernetesOutputInventory = InventoryKubernetesOutput
type KubernetesValidationInventory = InventoryKubernetesValidation

type InventoryFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// unitScope locates one rendered service unit: the workspace and module it
// belongs to, and the namespace its manifests bind to. The namespace is the
// module's (environments.Environment.ModuleNamespace) — the environment's own
// only when the workspace composes a single module — so every projection that
// stamps it (ExternalSecret, HPA, ServiceAccount, quota) agrees with the
// manifests the service agent rendered and with the addresses other modules
// were handed.
type unitScope struct {
	Workspace string
	Module    string
	Namespace string
	// Templates are the producer-declared assemblies of the secret keys this
	// render delivers. A key named here is assembled in the cluster from its
	// producer's primitives instead of read from the store as a whole.
	Templates renderTemplates
	// Managed are the environment's managed services, keyed as the environment
	// keys them. A managed producer's secret keys are enumerated there and
	// nowhere else, so resolving one of its primitives has to consult this
	// rather than EnvironmentServiceSecrets.
	Managed map[string]environments.EnvironmentManagedService
}

// moduleScope is the scope of every unit a module renders in an environment.
func moduleScope(env *environments.Environment, workspace *resources.Workspace, module string) unitScope {
	return unitScope{
		Workspace: workspace.Name,
		Module:    module,
		Namespace: env.ModuleNamespace(workspace, module),
		Managed:   env.ManagedServices,
	}
}

// secretScope is the scope a service's secret keys resolve in.
func (scope unitScope) secretScope(service string) environments.SecretScope {
	return environments.SecretScope{Workspace: scope.Workspace, Module: scope.Module, Service: service}
}

type RenderOptions struct {
	Destination string
	Module      string
	Unit        string
	UnitNames   []string
	Environment string
	Namespace   string
	AppProject  string
	Promotable  bool
	// CheckUnitDirectories asks validateTree to verify the on-disk unit
	// directories against Units. It is an explicit intent flag rather than an
	// inference from a populated field so the trigger and the data (Units)
	// cannot silently disagree.
	CheckUnitDirectories bool
	OwnedPath            string
	ModulePath           string
	Units                []InventoryUnit
	Package              *InventoryPackage
	// Pin is how the composition pinned the module, recorded in the inventory.
	Pin *InventoryPin
	// ReleaseArtifacts is what the module's package manifest declares the
	// release publishes; the runtime artifact named frontend among them is
	// the bundle a module manifest's generated frontend.payload names.
	ReleaseArtifacts []composition.ReleaseArtifact
	// Workspace is the composing workspace's name. A module manifest's binding ID
	// is scoped by it, so two workspaces delivering to one host never claim the
	// same binding.
	Workspace string
	// Host is the deployment host this render delivers to, as the environment
	// declares it. Absent, no module manifest is rendered: a manifest names
	// a host, and a derived coordinate is a guess the host refuses later.
	Host *environments.EnvironmentHost
	// Target is the environment a hosted render delivers to: the delivery
	// Jobs are declared against it.
	Target *environments.Environment
	// DeliveryTarget is the host's delivery API as this render resolved it from
	// the host block's delivery endpoint; the delivery Jobs POST to it.
	DeliveryTarget *DeliveryTarget
	// Delivered is what publish settled and signed, recorded in the inventory.
	Delivered *InventoryDelivery
	// WorkspaceConfigurationDigests are the digests of the workspace
	// configuration groups this render bakes in, recorded in the inventory.
	WorkspaceConfigurationDigests map[string]string
	// ModuleManifestPath is set by the render once it has written the
	// manifest, so the inventory records the delivered path rather than the
	// intent to write one.
	ModuleManifestPath string
	// HostsDelivery carries Inventory.HostsDelivery across a publish's
	// re-render of the inventory.
	HostsDelivery bool
	// Instance is the module instance whose workloads this render delivers,
	// resolved by the caller from the same resolution the workloads came from;
	// its manifest is rendered from it.
	Instance *ModuleInstance
	// UndeclaredManifest is why this render declares no module manifest, when
	// it declares none for a reason other than the environment naming no host.
	UndeclaredManifest string
	// MeshEdges are the edges into this module's workloads, recorded in the
	// inventory for the cell's applier.
	MeshEdges *MeshEdges
	// EndpointCarriers are the render's in-cluster addresses of the module's
	// routed endpoints, keyed as the gateway reads them: what its manifest's
	// endpoints carry.
	EndpointCarriers map[string]string
	renderRoot       *renderRoot
}

func inventoryKubernetesOutput(output *builderv0.DeploymentOutput) *InventoryKubernetesOutput {
	kubernetes := output.GetKubernetes()
	if kubernetes == nil {
		return nil
	}
	validation := kubernetes.GetValidation()
	if validation == nil {
		return nil
	}
	violations := append([]string{}, validation.GetViolations()...)
	return &InventoryKubernetesOutput{
		Kind:            kubernetes.GetKind().String(),
		Profile:         kubernetes.GetProfile().String(),
		ContractVersion: kubernetes.GetContractVersion(),
		Validation: &InventoryKubernetesValidation{
			StaticValidation:     validation.GetStaticValidation().String(),
			ServerSideValidation: validation.GetServerSideValidation().String(),
			Restricted:           validation.GetRestricted(),
			Violations:           violations,
		},
	}
}

type RenderResult struct {
	Path string `json:"path"`
	// Cell is the environment's cell file as re-rendered after this module,
	// when the environment declares a host.
	Cell *CellResult `json:"cell,omitempty"`
	// CellError says why the cell file could not be re-derived after this
	// render — a sibling module's tree this CLI cannot read, typically. The
	// module render stands; the cell file is stale until the named render.
	CellError string       `json:"cellError,omitempty"`
	Inventory Inventory    `json:"inventory"`
	Sizing    SizingReport `json:"sizing"`
	// ElidedNamespaces are the tree-relative manifest paths the render dropped
	// because they claimed the externally provisioned destination namespace. A
	// non-empty list means a service agent still emits the shared Namespace and
	// should be fixed; the render reports it rather than changing its committed
	// output silently. See service_namespace.go.
	ElidedNamespaces []string `json:"elidedNamespaces,omitempty"`
	// ClearedDev are the dev deployments the tree this render replaced carried.
	// A full render re-derives every image, so they no longer run.
	ClearedDev []InventoryDevDeployment `json:"clearedDev,omitempty"`
	// ModuleManifest is the module manifest this render declared: what it
	// says should be present on the host.
	ModuleManifest *DeclaredModuleManifest `json:"moduleManifest,omitempty"`
	// UndeclaredManifest says why the module declares no manifest when the
	// reason is the module's own: no package manifest to name a release from,
	// or no service that runs as the module.
	UndeclaredManifest string `json:"undeclaredManifest,omitempty"`
	// StaleGroupConsumers maps a workspace configuration group this render
	// consumes to the sibling modules whose rendered trees bake in another
	// value of it: rendered before the group changed, to be rendered next.
	// Reported here and refused at publish, where the sibling's render is the
	// action that lifts the refusal.
	StaleGroupConsumers map[string][]string `json:"staleGroupConsumers,omitempty"`
}

// InventoryDelivery is what publish settled into a tree: the delivery documents
// at their settled generations, and the identity that signed them.
type InventoryDelivery struct {
	// Signed reports whether every document carries a signature. Unsigned
	// documents are delivered only to a local qualification environment, and a
	// host refuses them.
	Signed bool `json:"signed"`
	// Identity is the envelope's claimed key ID when Signed. This field is
	// reporting only; the publisher and host verify the envelope separately.
	Identity string `json:"identity,omitempty"`
	// Documents are the module manifests delivered.
	Documents []InventoryDeliveredDocument `json:"documents,omitempty"`
}

// InventoryDeliveredDocument is one delivered document.
type InventoryDeliveredDocument struct {
	// Kind is "manifest".
	Kind string `json:"kind"`
	// ID is the binding the manifest declares.
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	// Removed marks a tombstone.
	Removed bool `json:"removed,omitempty"`
	// Digest is the document's canonical digest at this generation.
	Digest string `json:"digest"`
}

// DeclaredModuleManifest is the rendered module manifest: where it was
// written, which binding it declares, and at which provisional generation
// (publish settles the real one against the delivery repository).
type DeclaredModuleManifest struct {
	Path       string `json:"path"`
	Binding    string `json:"binding"`
	Generation uint64 `json:"generation"`
}

type PublishRequest struct {
	Module          string
	Environment     string
	PromotionBranch string
	CommitMessage   string
	Title           string
	Body            string
	Local           bool
	// AllowUnresolvedContracts downgrades a consumed-contract violation whose
	// exposing module is not yet deployed in the target GitOps tree from a
	// violation to a skipped check, for bootstrap ordering.
	AllowUnresolvedContracts bool
	// Signer signs the delivery documents this publish settles. nil means the
	// process environment decides: the platform's Vault Transit key, or
	// none — in which case only a Local publish may deliver unsigned.
	Signer signing.Signer `json:"-"`
	// Carriers are the carriers an inspected plan signed, for the publish
	// that executes the plan to deliver as they are. Set from the plan by
	// the command; never part of a request's own JSON.
	Carriers map[string][]byte `json:"-"`
	// Resign lets the plan sign again a document whose delivered carrier the
	// release policy no longer admits — after a signing identity rotated.
	// Deliberate: without it such a carrier refuses the publish by name.
	Resign bool `json:"resign,omitempty"`
	// AllowCellRemovals names the modules whose removal from the hosted
	// environment's cell file this publish intends: a namespace, workload,
	// endpoint or consumer the delivery base branch's cell file carries and
	// the workspace's does not. Any other removal refuses the publish.
	AllowCellRemovals []string `json:"allowCellRemovals,omitempty"`
}

type PublishPlan struct {
	ID              string `json:"id"`
	Repository      string `json:"repository"`
	RepositorySlug  string `json:"repositorySlug,omitempty"`
	Path            string `json:"path"`
	BaseBranch      string `json:"baseBranch"`
	BaseRevision    string `json:"baseRevision"`
	PromotionBranch string `json:"promotionBranch"`
	BranchRevision  string `json:"branchRevision,omitempty"`
	ExistingCommit  string `json:"existingCommit,omitempty"`
	// ReplacesBranchRevision is the tip of a stale promotion branch this
	// publish replaces: a branch that does not contain the base branch (its
	// pull request was squash-merged and the branch kept, or the base moved
	// past it), which the publish restarts from the base and pushes over with
	// a lease on exactly this revision. Part of the plan's identity, so a
	// branch that moves after review refuses the publish.
	ReplacesBranchRevision string          `json:"replacesBranchRevision,omitempty"`
	Module                 string          `json:"module"`
	Environment            string          `json:"environment"`
	RenderDigest           string          `json:"renderDigest"`
	SnapshotRevision       string          `json:"snapshotRevision"`
	Changed                []string        `json:"changed"`
	Diff                   string          `json:"diff"`
	ContractChecks         []ContractCheck `json:"contractChecks,omitempty"` // Delivery is what the publication settled and signed, for the plan's
	// reader and the result.
	Delivery *InventoryDelivery `json:"delivery,omitempty"`
	// StaleConsumers names the modules the delivery base branch holds at
	// another value of a workspace configuration group this publication bakes
	// in — published before the group changed, and the next publish to run.
	// Reported rather than refused: refusing would hold every consumer's
	// publish on every other's.
	StaleConsumers []string `json:"staleConsumers,omitempty"`
	// CellRemovals are the removals from the base branch's cell file the
	// request named (AllowCellRemovals), for the reviewer to see.
	CellRemovals []string `json:"cellRemovals,omitempty"`
	// Carriers are the carriers this plan signed, keyed by kind, ID and
	// generation. The publish that executes the plan delivers exactly these
	// bytes — signing is once, here — after holding each to its document.
	Carriers map[string][]byte `json:"carriers,omitempty"`
	// SelfPromotes marks a local publish to a k3d environment whose host
	// lists developers: there is no reviewer, the developer's signed publish
	// is the approval, and the publish fast-forwards the local delivery
	// repository's base branch to its commit itself (see promoteLocally).
	SelfPromotes bool `json:"selfPromotes,omitempty"`
	// LocalRepository is the repository on disk a self-promoting plan is
	// bound to; part of the plan's identity.
	LocalRepository *LocalRepositoryIdentity `json:"localRepository,omitempty"`
}

// ContractCheck reports the admission result of one consumed API contract
// against the exposing module's inventory in the same GitOps tree.
type ContractCheck struct {
	Unit     string `json:"unit"`
	Module   string `json:"module"`
	Service  string `json:"service"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"` // "ok" | "violation" | "skipped" | "drift"
	Message  string `json:"message,omitempty"`
}

type PublishMutation struct {
	Request PublishRequest `json:"request"`
	PlanID  string         `json:"planId"`
	// Carriers are the carriers the inspected plan signed, delivered by this
	// publish as they are; without them a release publish would sign again
	// and its tree would no longer be the plan's.
	Carriers map[string][]byte `json:"carriers,omitempty"`
}

type PublishResult struct {
	PlanID           string          `json:"planId"`
	Repository       string          `json:"repository"`
	Path             string          `json:"path"`
	BaseBranch       string          `json:"baseBranch"`
	PromotionBranch  string          `json:"promotionBranch"`
	RenderDigest     string          `json:"renderDigest"`
	SnapshotRevision string          `json:"snapshotRevision"`
	Commit           string          `json:"commit"`
	Tree             string          `json:"tree"`
	Signed           bool            `json:"signed"`
	PullRequest      string          `json:"pullRequest"`
	PullRequestID    int             `json:"pullRequestId,omitempty"`
	ContractChecks   []ContractCheck `json:"contractChecks,omitempty"`
	// Delivery is what this publish settled and signed: every module manifest
	// at its settled generation, and the identity that signed them.
	Delivery *InventoryDelivery `json:"delivery,omitempty"`
	// Promoted reports that this publish advanced the base branch itself: a
	// developer-signed local promotion (PublishPlan.SelfPromotes).
	Promoted bool `json:"promoted,omitempty"`
}

type RollbackRequest struct {
	PublishRequest
	ToRevision string `json:"toRevision"`
}

type RollbackPlan struct {
	PublishPlan
	ToRevision string `json:"toRevision"`
}

type RollbackMutation struct {
	Request  RollbackRequest   `json:"request"`
	PlanID   string            `json:"planId"`
	Carriers map[string][]byte `json:"carriers,omitempty"`
}

type ObserveRequest struct {
	WorkspaceRoot string
	Module        string
	Environment   string
	AppProject    string
	Applications  []string
	Repository    string
	Path          string
	Revision      string
	Commit        string
	Tree          string
	RenderDigest  string
	PullRequest   string
	Local         bool
	Timeout       time.Duration
	PollInterval  time.Duration
}

type ReviewEvidence struct {
	URL            string   `json:"url"`
	State          string   `json:"state"`
	ReviewDecision string   `json:"reviewDecision"`
	Reviewers      []string `json:"reviewers"`
	MergeCommit    string   `json:"mergeCommit"`
}

type ApplicationEvidence struct {
	Name                 string `json:"name"`
	Project              string `json:"project"`
	Repository           string `json:"repository"`
	Path                 string `json:"path"`
	Sync                 string `json:"sync"`
	Health               string `json:"health"`
	Operation            string `json:"operation"`
	Revision             string `json:"revision"`
	Cluster              string `json:"cluster"`
	ClusterIdentity      string `json:"clusterIdentity"`
	DestinationNamespace string `json:"destinationNamespace,omitempty"`
}

type Evidence struct {
	SchemaVersion   int                   `json:"schemaVersion"`
	Module          string                `json:"module"`
	Environment     string                `json:"environment"`
	RenderDigest    string                `json:"renderDigest"`
	SignedCommit    string                `json:"signedCommit"`
	Tree            string                `json:"tree"`
	Review          ReviewEvidence        `json:"review"`
	Repository      string                `json:"repository"`
	Path            string                `json:"path"`
	ArgoRevision    string                `json:"argoRevision"`
	Cluster         string                `json:"cluster"`
	ClusterIdentity string                `json:"clusterIdentity"`
	Health          string                `json:"health"`
	Applications    []ApplicationEvidence `json:"applications"`
	ObservedAt      time.Time             `json:"observedAt"`
}

type ObserveResult struct {
	Path     string   `json:"path"`
	Evidence Evidence `json:"evidence"`
}
