// Package deployment defines the offline execution-inventory contract. Validation
// establishes internal consistency against a caller-supplied context, never
// platform authorization. The caller must authenticate that context independently.
package deployment

const Schema = "codefly/execution-inventory/v1"
const KubernetesVersion = "1.34"
const ProfileVersion = "codefly/compiler-profile/v1"

type Inventory struct {
	Schema           string            `json:"schema"`
	Target           Target            `json:"target"`
	Delivery         string            `json:"delivery"`
	OwnershipDomain  string            `json:"ownership_domain"`
	Generation       int64             `json:"generation"`
	Previous         *Previous         `json:"previous"`
	EnvelopeRevision string            `json:"envelope_revision"`
	Complete         bool              `json:"complete"`
	Members          []Member          `json:"members"`
	Artifacts        []Artifact        `json:"artifacts"`
	Namespaces       []Namespace       `json:"namespaces"`
	Workloads        []Workload        `json:"workloads"`
	Dependencies     []Dependency      `json:"dependencies"`
	Ingress          []Ingress         `json:"ingress"`
	Egress           []Egress          `json:"egress"`
	ResourceBindings []ResourceBinding `json:"resource_bindings"`
	PlatformRefs     PlatformRefs      `json:"platform_refs"`
	Removed          bool              `json:"removed"`
}
type Target struct {
	Coordinate      string `json:"coordinate"`
	HostComponent   string `json:"host_component"`
	ClusterInstance string `json:"cluster_instance"`
}
type Previous struct {
	Generation int64  `json:"generation"`
	Digest     string `json:"digest"`
}

// Release is optional descriptive metadata, never the identity of an image.
type Release struct {
	Publisher string `json:"publisher"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}
type Member struct {
	Binding            string   `json:"binding"`
	PresenceGeneration int64    `json:"presence_generation"`
	DeclarationKind    string   `json:"declaration_kind"`
	Digest             string   `json:"digest"`
	Release            *Release `json:"release,omitempty"`
	Artifacts          []string `json:"artifacts"`
	Workloads          []string `json:"workloads"`
	Removed            bool     `json:"removed"`
}
type Artifact struct {
	ID      string           `json:"id"`
	Source  string           `json:"source"`
	Path    string           `json:"path"`
	Digest  string           `json:"digest"`
	Objects []ObjectIdentity `json:"objects"`
}
type ObjectIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}
type Namespace struct {
	Name          string `json:"name"`
	MemberBinding string `json:"member_binding"`
}
type Workload struct {
	ID                      string             `json:"id"`
	MemberBinding           string             `json:"member_binding"`
	Artifact                Origin             `json:"artifact"`
	Controller              ObjectIdentity     `json:"controller"`
	Selector                map[string]string  `json:"selector"`
	CredentialKind          string             `json:"credential_kind"`
	AuthenticatingContainer *string            `json:"authenticating_container"`
	Identity                *Identity          `json:"identity"`
	Template                Template           `json:"template"`
	ConfigurationRefs       []ConfigurationRef `json:"configuration_refs"`
	PlatformRoleRef         *string            `json:"platform_role_ref"`
}

// Origin has exactly one non-null alternative.
type Origin struct {
	Artifact       *string `json:"artifact"`
	CarrierProfile *string `json:"carrier_profile"`
}
type Identity struct {
	Issuer      string   `json:"issuer"`
	Audience    string   `json:"audience"`
	Subject     string   `json:"subject"`
	SPIFFEID    string   `json:"spiffe_id"`
	Attachments []string `json:"attachments"`
}
type Template struct {
	Metadata Metadata `json:"metadata"`
	Spec     PodSpec  `json:"spec"`
}
type Metadata struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// The v1 supported Kubernetes 1.34 subset is closed. All fields are explicit;
// neither an omitted field nor an unknown field can silently acquire semantics.
type PodSpec struct {
	ServiceAccountName            string            `json:"serviceAccountName"`
	AutomountServiceAccountToken  bool              `json:"automountServiceAccountToken"`
	RestartPolicy                 string            `json:"restartPolicy"`
	DNSPolicy                     string            `json:"dnsPolicy"`
	SchedulerName                 string            `json:"schedulerName"`
	TerminationGracePeriodSeconds int64             `json:"terminationGracePeriodSeconds"`
	EnableServiceLinks            bool              `json:"enableServiceLinks"`
	OS                            OS                `json:"os"`
	NodeSelector                  map[string]string `json:"nodeSelector"`
	Containers                    []Container       `json:"containers"`
	InitContainers                []Container       `json:"initContainers"`
	Volumes                       []Volume          `json:"volumes"`
}
type OS struct {
	Name string `json:"name"`
}
type Container struct {
	Name                     string          `json:"name"`
	Image                    string          `json:"image"`
	Command                  []string        `json:"command"`
	Args                     []string        `json:"args"`
	WorkingDir               string          `json:"workingDir"`
	Env                      []Env           `json:"env"`
	Ports                    []Port          `json:"ports"`
	VolumeMounts             []VolumeMount   `json:"volumeMounts"`
	ImagePullPolicy          string          `json:"imagePullPolicy"`
	RestartPolicy            *string         `json:"restartPolicy"`
	TerminationMessagePath   string          `json:"terminationMessagePath"`
	TerminationMessagePolicy string          `json:"terminationMessagePolicy"`
	SecurityContext          SecurityContext `json:"securityContext"`
}
type SecurityContext struct {
	RunAsNonRoot             bool           `json:"runAsNonRoot"`
	RunAsUser                int64          `json:"runAsUser"`
	RunAsGroup               int64          `json:"runAsGroup"`
	ReadOnlyRootFilesystem   bool           `json:"readOnlyRootFilesystem"`
	AllowPrivilegeEscalation bool           `json:"allowPrivilegeEscalation"`
	Privileged               bool           `json:"privileged"`
	Capabilities             Capabilities   `json:"capabilities"`
	SeccompProfile           SeccompProfile `json:"seccompProfile"`
}
type Capabilities struct {
	Drop []string `json:"drop"`
	Add  []string `json:"add"`
}
type SeccompProfile struct {
	Type string `json:"type"`
}
type Env struct {
	Name      string     `json:"name"`
	Value     *string    `json:"value"`
	ValueFrom *EnvSource `json:"valueFrom"`
}
type EnvSource struct {
	ConfigMapKeyRef *KeyRef `json:"configMapKeyRef"`
	SecretKeyRef    *KeyRef `json:"secretKeyRef"`
}
type KeyRef struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Optional bool   `json:"optional"`
}
type Port struct {
	Name          string `json:"name"`
	ContainerPort int64  `json:"containerPort"`
	Protocol      string `json:"protocol"`
}
type Volume struct {
	Name      string              `json:"name"`
	ConfigMap *VolumeSource       `json:"configMap"`
	Secret    *SecretVolumeSource `json:"secret"`
}
type VolumeSource struct {
	Name        string      `json:"name"`
	DefaultMode int64       `json:"defaultMode"`
	Optional    bool        `json:"optional"`
	Items       []KeyToPath `json:"items"`
}
type SecretVolumeSource struct {
	SecretName  string      `json:"secretName"`
	DefaultMode int64       `json:"defaultMode"`
	Optional    bool        `json:"optional"`
	Items       []KeyToPath `json:"items"`
}
type KeyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Mode int64  `json:"mode"`
}
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}
type ConfigurationRef struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"`
	Name       string             `json:"name"`
	CatalogRef string             `json:"catalog_ref"`
	Version    string             `json:"version"`
	Consumers  []ConfigurationUse `json:"consumers"`
}
type ConfigurationUse struct {
	Container string `json:"container"`
	Use       string `json:"use"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}
type EndpointRef struct {
	Workload *string `json:"workload"`
	Endpoint string  `json:"endpoint"`
}
type Dependency struct {
	Consumer string      `json:"consumer"`
	Provider EndpointRef `json:"provider"`
	Protocol string      `json:"protocol"`
	Port     int64       `json:"port"`
}
type Ingress struct {
	Workload string `json:"workload"`
	Endpoint string `json:"endpoint"`
	Host     string `json:"host"`
	Path     string `json:"path"`
	Exposure string `json:"exposure"`
}
type Egress struct {
	Host      string   `json:"host"`
	Protocol  string   `json:"protocol"`
	Port      int64    `json:"port"`
	CIDRs     []string `json:"cidrs"`
	Consumers []string `json:"consumers"`
}
type ResourceBinding struct {
	Workload string `json:"workload"`
	Resource string `json:"resource"`
	FactRef  string `json:"fact_ref"`
	GrantRef string `json:"grant_ref"`
}
type Reference struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}
type PlatformRefs struct {
	Execution Reference `json:"execution"`
	Identity  Reference `json:"identity"`
	Trust     Reference `json:"trust"`
	Compiler  Reference `json:"compiler"`
}

// Row is a projection, not an alternative editable source of execution facts.
// Row is EXACTLY the seven keys the cluster's admission policy compares, and
// nothing else. A test asserts the field count, deliberately: the row is the
// comparison, so a field that admission does not compare does not belong in
// it. To identify a row, see Projection.
type Row struct {
	Namespace      string            `json:"ns"`
	Labels         map[string]string `json:"labels"`
	ServiceAccount string            `json:"sa"`
	Container      *string           `json:"container"`
	Images         map[string]string `json:"images"`
	App            []string          `json:"app"`
	Init           []string          `json:"init"`
}

// Context is a distinct input. Content and Blobs retain exact bytes, encoded as
// base64 in JSON. Its provenance is the trusted caller's responsibility.
type Context struct {
	Schema    string            `json:"schema"`
	Documents []Document        `json:"documents"`
	Blobs     map[string][]byte `json:"blobs"`
}
type Document struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Content  []byte `json:"content"`
}
type ExecutionProfile struct {
	Schema          string         `json:"schema"`
	Platforms       []Platform     `json:"platforms"`
	CredentialKinds []string       `json:"credential_kinds"`
	Sources         []Source       `json:"sources"`
	Catalog         []CatalogEntry `json:"catalog"`
}
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}
type Source struct {
	ID       string `json:"id"`
	Location string `json:"location"`
}

// Catalog entries are exact scoped facts. Wildcards have no interpretation.
type CatalogEntry struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Namespace       string   `json:"namespace"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Digest          string   `json:"digest"`
	Workloads       []string `json:"workloads"`
	CredentialKinds []string `json:"credential_kinds"`
	Protocol        string   `json:"protocol"`
	Port            int64    `json:"port"`
}
type IdentityProfile struct {
	Schema string          `json:"schema"`
	Grants []IdentityGrant `json:"grants"`
}
type IdentityGrant struct {
	Workload       string   `json:"workload"`
	Namespace      string   `json:"namespace"`
	ServiceAccount string   `json:"service_account"`
	CredentialKind string   `json:"credential_kind"`
	Container      string   `json:"container"`
	Identity       Identity `json:"identity"`
}
type TrustProfile struct {
	Schema    string      `json:"schema"`
	Verifiers []Reference `json:"verifiers"`
}
type CompilerProfile struct {
	Schema            string   `json:"schema"`
	KubernetesVersion string   `json:"kubernetes_version"`
	Normalization     string   `json:"normalization"`
	SupportedKinds    []string `json:"supported_kinds"`
	ExcludedFields    []string `json:"excluded_fields"`
}

// Projection pairs a Row with the identity of what it projects. It exists so a
// consumer can select one delivery member's rows BY NAME rather than by
// position.
//
// The first consumer paired Rows()[i] with Inventory().Workloads[i] and said
// so before this module merged. That pairing is correct today and fragile
// forever: it holds only while the projection is one row per workload in
// workload order, so the first time a row is filtered, reordered or
// synthesised, every consumer silently mispairs — and mispairing a row means
// comparing one workload's observation against another workload's approved
// images, which is a wrong answer that looks like a right one.
//
// Identity lives here rather than in Row because Row is the admission
// comparison and must stay exactly those seven keys.
type Projection struct {
	Workload      string `json:"workload"`
	MemberBinding string `json:"member_binding"`
	Row           Row    `json:"row"`
}
