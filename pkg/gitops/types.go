package gitops

import "time"

const (
	InventoryFilename = ".codefly-render.json"
	SchemaVersion     = 2
)

type Inventory struct {
	SchemaVersion int                `json:"schemaVersion"`
	Module        string             `json:"module"`
	Service       string             `json:"service,omitempty"`
	Environment   string             `json:"environment"`
	AppProject    string             `json:"appProject"`
	OwnedPath     string             `json:"ownedPath"`
	ServiceGraph  []InventoryService `json:"serviceGraph"`
	Files         []InventoryFile    `json:"files"`
	Digest        string             `json:"digest"`
}

type InventoryService struct {
	Module  string                     `json:"module"`
	Service string                     `json:"service"`
	Path    string                     `json:"path,omitempty"`
	Managed bool                       `json:"managed,omitempty"`
	Output  *KubernetesOutputInventory `json:"output,omitempty"`
}

type KubernetesOutputInventory struct {
	Kind            string                        `json:"kind"`
	Profile         string                        `json:"profile"`
	ContractVersion string                        `json:"contractVersion"`
	Validation      KubernetesValidationInventory `json:"validation"`
}

type KubernetesValidationInventory struct {
	StaticValidation     string   `json:"staticValidation"`
	ServerSideValidation string   `json:"serverSideValidation"`
	Promotable           bool     `json:"promotable"`
	Violations           []string `json:"violations"`
}

type InventoryFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type RenderOptions struct {
	Destination  string
	Module       string
	Service      string
	Services     []string
	OwnedPath    string
	ServiceGraph []InventoryService
	Environment  string
	AppProject   string
	Promotable   bool
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
