package gitops

import (
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

const (
	InventoryFilename = ".codefly-render.json"
	SchemaVersion     = 6
	// priorSchemaVersion is the last schema loadInventory still accepts: a
	// render from before declared solution presence existed, carrying no
	// SolutionHostBindingPath.
	//
	// The bump is not cosmetic. A newer render's binding documents are wired
	// into Argo through this field, so an older CLI publishing that tree would
	// commit the documents and deliver them to nothing — silently. Refusing the
	// schema is what turns that into an error the operator sees.
	priorSchemaVersion    = 5
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
	// SolutionHostBindingPath is the render subdirectory holding the declared
	// SolutionHostBinding documents, when this render declared any. Publish
	// points an Argo Application at its environment overlay and derives the
	// promotion's authority from it, exactly as it does for a unit.
	SolutionHostBindingPath string `json:"solutionHostBindingPath,omitempty"`
	// Package identifies the module package this render was produced from
	// (module.package.codefly.yaml id/version), when the module has one.
	Package *InventoryPackage `json:"package,omitempty"`
	Units   []InventoryUnit   `json:"units"`
	Files   []InventoryFile   `json:"files"`
	Digest  string            `json:"digest"`
	// Dev lists the dev deployments (`codefly deploy dev`) applied on top of
	// this render: services whose image was re-pinned to code no release
	// describes. A full render re-derives the tree and never carries it over.
	Dev []InventoryDevDeployment `json:"dev,omitempty"`
}

type InventoryPackage struct {
	ID      string `json:"id"`
	Version string `json:"version"`
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

type InventoryKubernetesValidation struct {
	StaticValidation     string   `json:"staticValidation"`
	ServerSideValidation string   `json:"serverSideValidation"`
	Promotable           bool     `json:"promotable"`
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
	// Workspace is the composing workspace's name. A solution host binding ID
	// is scoped by it, so two workspaces delivering to one host never claim the
	// same binding.
	Workspace string
	// Host is the deployment host this render delivers to, as the environment
	// declares it. Absent, no solution host binding is rendered: a binding names
	// a host, and a derived coordinate is a guess the host refuses later.
	Host *environments.EnvironmentHost
	// SolutionHostBindingPath is set by the render once it has written binding
	// documents, so the inventory records the delivered path rather than the
	// intent to write one.
	SolutionHostBindingPath string
	// SolutionInstances are the solution instances of the resolved composition
	// whose workloads this render delivers, resolved by the caller from the same
	// resolution the workloads came from. One SolutionHostBinding is rendered
	// per entry.
	SolutionInstances []SolutionInstance
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
			Promotable:           validation.GetPromotable(),
			Violations:           violations,
		},
	}
}

type RenderResult struct {
	Path      string       `json:"path"`
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
	// SolutionHostBindings are the tree-relative paths of the rendered
	// SolutionHostBinding documents: what this render declares should be
	// present on the host, one per solution instance.
	SolutionHostBindings []string `json:"solutionHostBindings,omitempty"`
	// UndeclaredSolutions are the solution instances this render delivered
	// workloads for but declared no binding for, because the environment names
	// no host. It is reported rather than inferred from an empty list: a
	// missing declaration and a composition with no solution look the same
	// otherwise.
	UndeclaredSolutions []string `json:"undeclaredSolutions,omitempty"`
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
}

type PublishPlan struct {
	ID               string          `json:"id"`
	Repository       string          `json:"repository"`
	RepositorySlug   string          `json:"repositorySlug,omitempty"`
	Path             string          `json:"path"`
	BaseBranch       string          `json:"baseBranch"`
	BaseRevision     string          `json:"baseRevision"`
	PromotionBranch  string          `json:"promotionBranch"`
	BranchRevision   string          `json:"branchRevision,omitempty"`
	ExistingCommit   string          `json:"existingCommit,omitempty"`
	Module           string          `json:"module"`
	Environment      string          `json:"environment"`
	RenderDigest     string          `json:"renderDigest"`
	SnapshotRevision string          `json:"snapshotRevision"`
	Changed          []string        `json:"changed"`
	Diff             string          `json:"diff"`
	ContractChecks   []ContractCheck `json:"contractChecks,omitempty"`
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
	Request RollbackRequest `json:"request"`
	PlanID  string          `json:"planId"`
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
