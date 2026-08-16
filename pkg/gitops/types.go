package gitops

import (
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

const (
	InventoryFilename     = ".codefly-render.json"
	SchemaVersion         = 4
	EvidenceSchemaVersion = 1
)

// UnitKindService is the artifact kind for a codefly service. It is the only
// kind rendered today; new kinds (jobs, applications, solutions) register their
// own render subdirectory in unitDirectory.
const UnitKindService = "service"

// moduleBundleDir is the render subdirectory holding the module-level bundle. It
// sits beside the per-unit directories but is not itself a unit, so it is not
// derived from unitDirectory.
const moduleBundleDir = "module"

// unitDirectory maps an artifact kind to the render subdirectory that holds its
// units, reporting whether the kind is known. Generalizing the render path
// beyond services is a matter of adding a case here rather than threading a new
// shape through the inventory. The ok return keeps callers from ever joining an
// empty segment for an unrecognized kind.
func unitDirectory(kind string) (string, bool) {
	switch kind {
	case UnitKindService:
		return "services", true
	default:
		return "", false
	}
}

type Inventory struct {
	SchemaVersion int             `json:"schemaVersion"`
	Module        string          `json:"module"`
	Unit          string          `json:"unit,omitempty"`
	Environment   string          `json:"environment"`
	Namespace     string          `json:"namespace,omitempty"`
	AppProject    string          `json:"appProject"`
	OwnedPath     string          `json:"ownedPath"`
	ModulePath    string          `json:"modulePath,omitempty"`
	Units         []InventoryUnit `json:"units"`
	Files         []InventoryFile `json:"files"`
	Digest        string          `json:"digest"`
}

type InventoryUnit struct {
	Kind      string                     `json:"kind"`
	Module    string                     `json:"module"`
	Name      string                     `json:"name"`
	Path      string                     `json:"path,omitempty"`
	Managed   bool                       `json:"managed,omitempty"`
	Bootstrap bool                       `json:"bootstrap,omitempty"`
	Output    *InventoryKubernetesOutput `json:"output,omitempty"`
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
	Path      string    `json:"path"`
	Inventory Inventory `json:"inventory"`
}

type PublishRequest struct {
	Module          string
	Environment     string
	PromotionBranch string
	CommitMessage   string
	Title           string
	Body            string
	Local           bool
}

type PublishPlan struct {
	ID               string   `json:"id"`
	Repository       string   `json:"repository"`
	RepositorySlug   string   `json:"repositorySlug,omitempty"`
	Path             string   `json:"path"`
	BaseBranch       string   `json:"baseBranch"`
	BaseRevision     string   `json:"baseRevision"`
	PromotionBranch  string   `json:"promotionBranch"`
	BranchRevision   string   `json:"branchRevision,omitempty"`
	ExistingCommit   string   `json:"existingCommit,omitempty"`
	Module           string   `json:"module"`
	Environment      string   `json:"environment"`
	RenderDigest     string   `json:"renderDigest"`
	SnapshotRevision string   `json:"snapshotRevision"`
	Changed          []string `json:"changed"`
	Diff             string   `json:"diff"`
}

type PublishMutation struct {
	Request PublishRequest `json:"request"`
	PlanID  string         `json:"planId"`
}

type PublishResult struct {
	PlanID           string `json:"planId"`
	Repository       string `json:"repository"`
	Path             string `json:"path"`
	BaseBranch       string `json:"baseBranch"`
	PromotionBranch  string `json:"promotionBranch"`
	RenderDigest     string `json:"renderDigest"`
	SnapshotRevision string `json:"snapshotRevision"`
	Commit           string `json:"commit"`
	Tree             string `json:"tree"`
	Signed           bool   `json:"signed"`
	PullRequest      string `json:"pullRequest"`
	PullRequestID    int    `json:"pullRequestId,omitempty"`
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
