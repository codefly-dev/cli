package gitops

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// A local read-only fetch remote is a container the CLI owns on the private k3d
// network so Argo CD can fetch the reviewed Git revision without a wildcard host
// port, a mutable image, an ad-hoc certificate, or /tmp state. It serves an
// exact, immutable revision from a read-only bare mirror over HTTPS and is torn
// down only after every ownership and network identity is re-validated.
//
// Plugins never see any of this: the remote mirrors a repository the promotion
// layer already produced and speaks only Git, TLS, and Docker here in the CLI.
const (
	// remoteRole marks every object this lifecycle owns. Teardown refuses to
	// mutate a container that does not carry it.
	remoteRole = "gitops-fetch-remote"

	// remoteImage is the digest-pinned runtime image. Serving is dumb HTTP over
	// TLS from a read-only tree, so a static file server is enough. Pin only by
	// digest — a floating tag is a mutable image, which NewRemoteSpec rejects.
	// This digest is a placeholder pending a reviewed pin; operators override it
	// with CODEFLY_GITOPS_REMOTE_IMAGE, and the disposable k3d qualification
	// pins the digest it actually pulled.
	remoteImage = "nginx:1.27.3-alpine@sha256:41523187cf7d7a2f2677a80609d9caa14388bf5c1fbca9c410ba3de602aaaab4"

	remoteContainerPort = 443
	remoteMountRepo     = "/srv/repo.git"
	remoteMountTLS      = "/tls"
	remoteMountConf     = "/etc/nginx/nginx.conf"
	remoteServeRef      = "refs/heads/codefly-fetch"

	defaultCertValidity     = 24 * time.Hour
	defaultCertRotateBefore = 6 * time.Hour

	labelPrefix        = "codefly.gitops."
	labelRole          = labelPrefix + "role"
	labelOwner         = labelPrefix + "owner"
	labelWorkspace     = labelPrefix + "workspace"
	labelEnvironment   = labelPrefix + "environment"
	labelCluster       = labelPrefix + "cluster"
	labelRepository    = labelPrefix + "repository"
	labelRevision      = labelPrefix + "revision"
	labelCertNotAfter  = labelPrefix + "cert-not-after"
	labelCAFingerprint = labelPrefix + "ca-fingerprint"
)

func remoteImageDefault() string {
	if override := strings.TrimSpace(os.Getenv("CODEFLY_GITOPS_REMOTE_IMAGE")); override != "" {
		return override
	}
	return remoteImage
}

// RemoteConfig is the environment-scoped input to the fetch-remote lifecycle.
// Everything except the served revision is stable for the lifetime of the
// environment, so status and teardown can rebuild the exact identity without a
// module publication.
type RemoteConfig struct {
	WorkspaceRoot    string
	Owner            string
	Workspace        string
	Environment      string
	Cluster          string
	Network          string
	RepositorySlug   string
	SourceRepo       string
	Image            string
	HostAddr         string
	HostPort         int
	CertValidity     time.Duration
	CertRotateBefore time.Duration
}

// RemoteSpec is the deterministic identity and runtime plan derived from a
// RemoteConfig. It carries no revision: the revision is a per-publication input
// to Up and is recorded on the container and receipt.
type RemoteSpec struct {
	Owner            string
	Workspace        string
	Environment      string
	Cluster          string
	RepositorySlug   string
	SourceRepo       string
	WorkspaceRoot    string
	Network          string
	ContainerName    string
	DNSName          string
	Image            string
	HostAddr         string
	HostPort         int
	ContainerPort    int
	CertValidity     time.Duration
	CertRotateBefore time.Duration
}

func dnsUnsafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		return false
	default:
		return true
	}
}

func sanitizeDNSLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Map(func(r rune) rune {
		if dnsUnsafe(r) {
			return '-'
		}
		return r
	}, value)
	return strings.Trim(value, "-")
}

// NewRemoteSpec validates a RemoteConfig, applies defaults, and derives the
// deterministic container identity. The identity depends only on stable fields
// so Up and Down agree on the exact object without extra state.
func NewRemoteSpec(cfg *RemoteConfig) (RemoteSpec, error) {
	root := strings.TrimSpace(cfg.WorkspaceRoot)
	if root == "" {
		return RemoteSpec{}, fmt.Errorf("workspace root is required")
	}
	owner := sanitizeDNSLabel(cfg.Owner)
	if owner == "" {
		return RemoteSpec{}, fmt.Errorf("owner is required")
	}
	environment := sanitizeDNSLabel(cfg.Environment)
	if environment == "" {
		return RemoteSpec{}, fmt.Errorf("environment is required")
	}
	cluster := sanitizeDNSLabel(cfg.Cluster)
	if cluster == "" {
		return RemoteSpec{}, fmt.Errorf("k3d cluster is required for a local fetch remote")
	}
	if strings.TrimSpace(cfg.RepositorySlug) == "" {
		return RemoteSpec{}, fmt.Errorf("repository slug is required")
	}
	if strings.TrimSpace(cfg.SourceRepo) == "" {
		return RemoteSpec{}, fmt.Errorf("source repository is required")
	}
	network := strings.TrimSpace(cfg.Network)
	if network == "" {
		network = "k3d-" + cluster
	}
	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = remoteImageDefault()
	}
	if !strings.Contains(image, "@sha256:") {
		return RemoteSpec{}, fmt.Errorf("runtime image %q must be pinned by digest", image)
	}
	hostAddr := strings.TrimSpace(cfg.HostAddr)
	if hostAddr == "" {
		hostAddr = "127.0.0.1"
	}
	if ip := net.ParseIP(hostAddr); ip == nil || !ip.IsLoopback() {
		return RemoteSpec{}, fmt.Errorf("host verification address %q must be IPv4 loopback", hostAddr)
	}
	identity := strings.Join([]string{owner, sanitizeDNSLabel(cfg.Workspace), environment, cluster, cfg.RepositorySlug}, "|")
	sum := sha256.Sum256([]byte(identity))
	short := hex.EncodeToString(sum[:])[:8]
	name := sanitizeDNSLabel("codefly-gitops-remote-" + environment + "-" + short)
	if len(name) > 63 {
		name = name[:63]
	}
	hostPort := cfg.HostPort
	if hostPort == 0 {
		hostPort = 49152 + int(binary.BigEndian.Uint16(sum[8:10]))%8192
	}
	if hostPort < 1 || hostPort > 65535 {
		return RemoteSpec{}, fmt.Errorf("host port %d is out of range", hostPort)
	}
	validity := cfg.CertValidity
	if validity <= 0 {
		validity = defaultCertValidity
	}
	rotateBefore := cfg.CertRotateBefore
	if rotateBefore <= 0 {
		rotateBefore = defaultCertRotateBefore
	}
	if rotateBefore >= validity {
		return RemoteSpec{}, fmt.Errorf("certificate rotation window %s must be shorter than its validity %s", rotateBefore, validity)
	}
	return RemoteSpec{
		Owner:            owner,
		Workspace:        sanitizeDNSLabel(cfg.Workspace),
		Environment:      environment,
		Cluster:          cluster,
		RepositorySlug:   cfg.RepositorySlug,
		SourceRepo:       cfg.SourceRepo,
		WorkspaceRoot:    root,
		Network:          network,
		ContainerName:    name,
		DNSName:          name,
		Image:            image,
		HostAddr:         hostAddr,
		HostPort:         hostPort,
		ContainerPort:    remoteContainerPort,
		CertValidity:     validity,
		CertRotateBefore: rotateBefore,
	}, nil
}

func (s *RemoteSpec) StateDir() string {
	return filepath.Join(s.WorkspaceRoot, ".codefly", "gitops", "remote", s.Environment)
}

func (s *RemoteSpec) RepoDir() string  { return filepath.Join(s.StateDir(), "repo.git") }
func (s *RemoteSpec) TLSDir() string   { return filepath.Join(s.StateDir(), "tls") }
func (s *RemoteSpec) ConfPath() string { return filepath.Join(s.StateDir(), "nginx.conf") }
func (s *RemoteSpec) receiptPath() string {
	return filepath.Join(s.StateDir(), "remote.json")
}

// ArgoRepository is the private, in-cluster URL Argo fetches over. It resolves
// through container/Kubernetes DNS and never traverses a host port.
func (s *RemoteSpec) ArgoRepository() string {
	return "https://" + s.DNSName + "/repo.git"
}

// HostVerifyURL is the loopback-only endpoint the CLI probes; it is never an
// Argo fetch path.
func (s *RemoteSpec) HostVerifyURL() string {
	return fmt.Sprintf("https://%s:%d/repo.git/info/refs?service=git-upload-pack", s.HostAddr, s.HostPort)
}

// Labels are the exact ownership markers stamped on the container. Teardown and
// doctor validate every one of them before touching the object.
func (s *RemoteSpec) Labels(revision, caFingerprint string, notAfter time.Time) map[string]string {
	return map[string]string{
		labelRole:          remoteRole,
		labelOwner:         s.Owner,
		labelWorkspace:     s.Workspace,
		labelEnvironment:   s.Environment,
		labelCluster:       s.Cluster,
		labelRepository:    s.RepositorySlug,
		labelRevision:      revision,
		labelCertNotAfter:  notAfter.UTC().Format(time.RFC3339),
		labelCAFingerprint: caFingerprint,
	}
}

// dockerRunArgs builds the exact `docker run` invocation. Every security
// invariant the issue names is encoded here so tests assert it directly: a
// loopback-only host binding, the private network, read-only mounts, a
// digest-pinned image, and exact ownership labels.
func (s *RemoteSpec) dockerRunArgs(labels map[string]string) []string {
	args := []string{
		"run", "--detach",
		"--name", s.ContainerName,
		"--network", s.Network,
		"--restart", "unless-stopped",
		"--read-only",
		"--publish", fmt.Sprintf("%s:%d:%d", s.HostAddr, s.HostPort, s.ContainerPort),
	}
	for _, writable := range []string{"/var/cache/nginx", "/var/run", "/tmp"} {
		args = append(args, "--tmpfs", writable)
	}
	for _, mount := range []string{
		s.RepoDir() + ":" + remoteMountRepo + ":ro",
		s.TLSDir() + ":" + remoteMountTLS + ":ro",
		s.ConfPath() + ":" + remoteMountConf + ":ro",
	} {
		args = append(args, "--volume", mount)
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	return append(args, s.Image)
}

func (s *RemoteSpec) nginxConfig() string {
	return `worker_processes 1;
pid /var/run/nginx.pid;
events { worker_connections 64; }
http {
  access_log off;
  server_tokens off;
  server {
    listen ` + fmt.Sprint(remoteContainerPort) + ` ssl;
    ssl_certificate ` + remoteMountTLS + `/server.crt;
    ssl_certificate_key ` + remoteMountTLS + `/server.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    root /srv;
    location / {
      autoindex off;
      limit_except GET HEAD { deny all; }
    }
  }
}
`
}

// RemotePlan is the read-only description of what Up would create.
type RemotePlan struct {
	ContainerName  string            `json:"containerName"`
	Image          string            `json:"image"`
	Network        string            `json:"network"`
	HostBinding    string            `json:"hostBinding"`
	ArgoRepository string            `json:"argoRepository"`
	SourceRepo     string            `json:"sourceRepo"`
	Revision       string            `json:"revision,omitempty"`
	Mounts         []string          `json:"mounts"`
	Labels         map[string]string `json:"labels"`
	CertValidity   string            `json:"certValidity"`
}

// Plan describes the intended remote without touching Docker.
func (s *RemoteSpec) Plan(revision string) RemotePlan {
	return RemotePlan{
		ContainerName:  s.ContainerName,
		Image:          s.Image,
		Network:        s.Network,
		HostBinding:    fmt.Sprintf("%s:%d -> %d/tcp", s.HostAddr, s.HostPort, s.ContainerPort),
		ArgoRepository: s.ArgoRepository(),
		SourceRepo:     s.SourceRepo,
		Revision:       revision,
		Mounts: []string{
			s.RepoDir() + ":" + remoteMountRepo + ":ro",
			s.TLSDir() + ":" + remoteMountTLS + ":ro",
			s.ConfPath() + ":" + remoteMountConf + ":ro",
		},
		Labels:       s.Labels(revision, "", time.Time{}),
		CertValidity: s.CertValidity.String(),
	}
}

// TLSMaterial is the generated, out-of-Git trust chain. The CA PEM is the
// declarative material Argo trusts; the private keys never leave owner-only
// files and are never returned as bytes or logged.
type TLSMaterial struct {
	CACertPath     string
	ServerCertPath string
	ServerKeyPath  string
	CAPEM          []byte
	CAFingerprint  string
	NotAfter       time.Time
	Rotated        bool
}

// ensureTLSMaterial generates or rotates the trust chain under dir. It reuses an
// existing server certificate only when it still covers the exact SANs and is
// not inside its rotation window; otherwise it mints a fresh CA and leaf. Keys
// are written 0600 in a 0700 directory and are never printed.
func ensureTLSMaterial(dir string, spec *RemoteSpec, now time.Time) (TLSMaterial, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return TLSMaterial{}, err
	}
	caCertPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")
	serverCertPath := filepath.Join(dir, "server.crt")
	serverKeyPath := filepath.Join(dir, "server.key")

	if existing, ok := loadUsableServer(caCertPath, serverCertPath, spec, now); ok {
		return existing, nil
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSMaterial{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "codefly gitops fetch remote CA " + spec.Environment},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(spec.CertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return TLSMaterial{}, err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSMaterial{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return TLSMaterial{}, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: spec.DNSName},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(spec.CertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{spec.DNSName},
		IPAddresses:  []net.IP{net.ParseIP(spec.HostAddr)},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return TLSMaterial{}, err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if err := writeFileMode(caCertPath, caPEM, 0o644); err != nil {
		return TLSMaterial{}, err
	}
	if err := writePrivateKey(caKeyPath, caKey); err != nil {
		return TLSMaterial{}, err
	}
	if err := writeFileMode(serverCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0o644); err != nil {
		return TLSMaterial{}, err
	}
	if err := writePrivateKey(serverKeyPath, serverKey); err != nil {
		return TLSMaterial{}, err
	}
	return TLSMaterial{
		CACertPath:     caCertPath,
		ServerCertPath: serverCertPath,
		ServerKeyPath:  serverKeyPath,
		CAPEM:          caPEM,
		CAFingerprint:  fingerprint(caDER),
		NotAfter:       caCert.NotAfter,
		Rotated:        true,
	}, nil
}

func loadUsableServer(caCertPath, serverCertPath string, spec *RemoteSpec, now time.Time) (TLSMaterial, bool) {
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return TLSMaterial{}, false
	}
	serverPEM, err := os.ReadFile(serverCertPath)
	if err != nil {
		return TLSMaterial{}, false
	}
	caBlock, _ := pem.Decode(caPEM)
	serverBlock, _ := pem.Decode(serverPEM)
	if caBlock == nil || serverBlock == nil {
		return TLSMaterial{}, false
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return TLSMaterial{}, false
	}
	serverCert, err := x509.ParseCertificate(serverBlock.Bytes)
	if err != nil {
		return TLSMaterial{}, false
	}
	if now.Add(spec.CertRotateBefore).After(serverCert.NotAfter) {
		return TLSMaterial{}, false
	}
	if len(serverCert.DNSNames) != 1 || serverCert.DNSNames[0] != spec.DNSName {
		return TLSMaterial{}, false
	}
	if len(serverCert.IPAddresses) != 1 || !serverCert.IPAddresses[0].Equal(net.ParseIP(spec.HostAddr)) {
		return TLSMaterial{}, false
	}
	return TLSMaterial{
		CACertPath:     caCertPath,
		ServerCertPath: serverCertPath,
		ServerKeyPath:  filepath.Join(filepath.Dir(serverCertPath), "server.key"),
		CAPEM:          caPEM,
		CAFingerprint:  fingerprint(caBlock.Bytes),
		NotAfter:       caCert.NotAfter,
	}, true
}

func writePrivateKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writeFileMode(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func serial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	value, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(1)
	}
	return value
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PortBinding is one host binding of a container port.
type PortBinding struct {
	ContainerPort string
	HostIP        string
	HostPort      string
}

// ContainerState is the subset of `docker inspect` the lifecycle reasons about.
type ContainerState struct {
	Name         string
	Running      bool
	Image        string
	Labels       map[string]string
	Networks     []string
	PortBindings []PortBinding
	Exists       bool
}

func parseContainerState(data []byte) (ContainerState, error) {
	var inspected []struct {
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		Image           string `json:"Image"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
		HostConfig struct {
			PortBindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(data, &inspected); err != nil {
		return ContainerState{}, err
	}
	if len(inspected) == 0 {
		return ContainerState{}, fmt.Errorf("empty docker inspect result")
	}
	entry := inspected[0]
	state := ContainerState{
		Name:    strings.TrimPrefix(entry.Name, "/"),
		Running: entry.State.Running,
		Image:   entry.Config.Image,
		Labels:  entry.Config.Labels,
		Exists:  true,
	}
	if state.Image == "" {
		state.Image = entry.Image
	}
	for network := range entry.NetworkSettings.Networks {
		state.Networks = append(state.Networks, network)
	}
	sort.Strings(state.Networks)
	for port, bindings := range entry.HostConfig.PortBindings {
		for _, binding := range bindings {
			state.PortBindings = append(state.PortBindings, PortBinding{
				ContainerPort: port,
				HostIP:        binding.HostIP,
				HostPort:      binding.HostPort,
			})
		}
	}
	sort.Slice(state.PortBindings, func(i, j int) bool {
		return state.PortBindings[i].ContainerPort < state.PortBindings[j].ContainerPort
	})
	return state, nil
}

// RemoteFinding is one validation result. Doctor and status render these.
type RemoteFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

const (
	severityWarn = "warn"
	severityFail = "fail"

	codeOwnershipDrift = "ownership-drift"
	codeNetworkDrift   = "network-drift"

	wildcardIPv4Host = "0.0.0.0"
)

func isWildcardHost(hostIP string) bool {
	switch strings.TrimSpace(hostIP) {
	case "", wildcardIPv4Host, "::", "[::]":
		return true
	default:
		return false
	}
}

// AuditState reports the drift a container leaks on its own, without a spec:
// wildcard host bindings, a mutable (non-digest) image, an expired certificate,
// or a stopped remote. Doctor uses this to sweep every labeled remote it finds.
func AuditState(state *ContainerState, now time.Time) []RemoteFinding {
	var findings []RemoteFinding
	if state.Labels[labelRole] != remoteRole {
		return findings
	}
	for _, binding := range state.PortBindings {
		if isWildcardHost(binding.HostIP) {
			findings = append(findings, RemoteFinding{
				Code:     "wildcard-binding",
				Severity: severityFail,
				Message:  fmt.Sprintf("%s publishes %s on wildcard host %q; bind IPv4 loopback only", state.Name, binding.ContainerPort, binding.HostIP),
			})
		}
	}
	if !strings.Contains(state.Image, "@sha256:") {
		findings = append(findings, RemoteFinding{
			Code:     "mutable-image",
			Severity: severityFail,
			Message:  fmt.Sprintf("%s runs mutable image %q; pin by digest", state.Name, state.Image),
		})
	}
	if raw := state.Labels[labelCertNotAfter]; raw != "" {
		if notAfter, err := time.Parse(time.RFC3339, raw); err == nil && !now.Before(notAfter) {
			findings = append(findings, RemoteFinding{
				Code:     "expired-certificate",
				Severity: severityFail,
				Message:  fmt.Sprintf("%s certificate expired at %s", state.Name, raw),
			})
		}
	}
	if !state.Running {
		findings = append(findings, RemoteFinding{
			Code:     "not-running",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%s is not running", state.Name),
		})
	}
	return findings
}

// InspectFindings validates a container against the exact spec and served
// revision: ownership drift, network drift, missing CA trust, a stale revision,
// plus everything AuditState covers.
func InspectFindings(spec *RemoteSpec, state *ContainerState, revision string, now time.Time) []RemoteFinding {
	if !state.Exists {
		return []RemoteFinding{{Code: "absent", Severity: severityWarn, Message: "fetch remote is not created"}}
	}
	findings := AuditState(state, now)
	if state.Labels[labelRole] != remoteRole {
		return append(findings, RemoteFinding{
			Code:     codeOwnershipDrift,
			Severity: severityFail,
			Message:  fmt.Sprintf("%s is missing the %s ownership marker", state.Name, labelRole),
		})
	}
	for label, want := range map[string]string{
		labelOwner:       spec.Owner,
		labelWorkspace:   spec.Workspace,
		labelEnvironment: spec.Environment,
		labelCluster:     spec.Cluster,
		labelRepository:  spec.RepositorySlug,
	} {
		if got := state.Labels[label]; got != want {
			findings = append(findings, RemoteFinding{
				Code:     codeOwnershipDrift,
				Severity: severityFail,
				Message:  fmt.Sprintf("%s label %s=%q, expected %q", state.Name, label, got, want),
			})
		}
	}
	if !slices.Contains(state.Networks, spec.Network) {
		findings = append(findings, RemoteFinding{
			Code:     codeNetworkDrift,
			Severity: severityFail,
			Message:  fmt.Sprintf("%s is not attached to %s (networks: %s)", state.Name, spec.Network, strings.Join(state.Networks, ",")),
		})
	}
	if state.Labels[labelCAFingerprint] == "" {
		findings = append(findings, RemoteFinding{
			Code:     "missing-ca-trust",
			Severity: severityFail,
			Message:  fmt.Sprintf("%s carries no CA fingerprint; Argo repository-CA trust cannot be verified", state.Name),
		})
	}
	if revision != "" && state.Labels[labelRevision] != revision {
		findings = append(findings, RemoteFinding{
			Code:     "stale-remote",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%s serves %q, expected reviewed revision %q", state.Name, state.Labels[labelRevision], revision),
		})
	}
	return findings
}

// validateOwnership refuses teardown when a container's identity or network
// membership has drifted from the spec — the guard that turns a silent partial
// deletion into an explicit refusal.
func validateOwnership(spec *RemoteSpec, state *ContainerState) error {
	for _, finding := range InspectFindings(spec, state, "", time.Now()) {
		switch finding.Code {
		case codeOwnershipDrift, codeNetworkDrift:
			return fmt.Errorf("refusing to tear down %s: %s", state.Name, finding.Message)
		}
	}
	return nil
}

// FetchRemote drives the environment-scoped lifecycle over Docker and Git.
type FetchRemote struct {
	Spec RemoteSpec
}

// NewFetchRemote resolves the fetch-remote identity from the workspace and
// environment. It refuses non-k3d environments: this lifecycle owns only the
// local, disposable read-only remote.
func NewFetchRemote(workspace *resources.Workspace, environment string) (*FetchRemote, error) {
	if workspace == nil {
		return nil, fmt.Errorf("workspace is required")
	}
	env, err := orchestration.SelectEnvironment(workspace, environment)
	if err != nil {
		return nil, err
	}
	if !env.IsK3d() {
		return nil, fmt.Errorf("environment %q is not k3d; a local fetch remote applies to k3d only", environment)
	}
	config, slug, _, _, err := resolveGitops(workspace, environment, true)
	if err != nil {
		return nil, err
	}
	cluster := ""
	if env.Cluster != nil {
		cluster = strings.TrimPrefix(strings.TrimSpace(env.Cluster.Context), "k3d-")
	}
	if cluster == "" {
		cluster = sanitizeDNSLabel(workspace.Name + "-" + environment)
	}
	spec, err := NewRemoteSpec(&RemoteConfig{
		WorkspaceRoot:  workspace.Dir(),
		Owner:          currentOwner(),
		Workspace:      workspace.Name,
		Environment:    environment,
		Cluster:        cluster,
		RepositorySlug: slug,
		SourceRepo:     config.RepoURL,
	})
	if err != nil {
		return nil, err
	}
	return &FetchRemote{Spec: spec}, nil
}

func currentOwner() string {
	for _, key := range []string{"CODEFLY_GITOPS_OWNER", "USER", "LOGNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "codefly"
}

// remoteReceipt is the auditable record of the last Up.
type remoteReceipt struct {
	Spec           RemoteSpec `json:"spec"`
	Revision       string     `json:"revision"`
	ArgoRepository string     `json:"argoRepository"`
	CAFingerprint  string     `json:"caFingerprint"`
	CACertPath     string     `json:"caCertPath"`
	CertNotAfter   time.Time  `json:"certNotAfter"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// RemoteStatus is the observed state plus the validation findings.
type RemoteStatus struct {
	Spec           RemoteSpec      `json:"spec"`
	State          ContainerState  `json:"state"`
	Revision       string          `json:"revision,omitempty"`
	ArgoRepository string          `json:"argoRepository"`
	CACertPath     string          `json:"caCertPath,omitempty"`
	CAFingerprint  string          `json:"caFingerprint,omitempty"`
	Findings       []RemoteFinding `json:"findings"`
}

// Plan returns the intended remote for the reviewed revision without side
// effects.
func (r *FetchRemote) Plan(revision string) RemotePlan {
	return r.Spec.Plan(revision)
}

// Up mirrors the reviewed revision into a read-only bare repository, ensures the
// out-of-Git trust chain, and (re)creates the container with a loopback-only
// host binding on the private network. It is idempotent and re-validates
// ownership before replacing an existing container.
func (r *FetchRemote) Up(ctx context.Context, revision string) (RemoteStatus, error) {
	revision = strings.ToLower(strings.TrimSpace(revision))
	if !gitObjectPattern.MatchString(revision) {
		return RemoteStatus{}, fmt.Errorf("revision must be an exact Git object ID")
	}
	if err := os.MkdirAll(r.Spec.StateDir(), 0o700); err != nil {
		return RemoteStatus{}, err
	}
	if err := r.mirror(ctx, revision); err != nil {
		return RemoteStatus{}, err
	}
	material, err := ensureTLSMaterial(r.Spec.TLSDir(), &r.Spec, time.Now())
	if err != nil {
		return RemoteStatus{}, err
	}
	if err := writeFileMode(r.Spec.ConfPath(), []byte(r.Spec.nginxConfig()), 0o644); err != nil {
		return RemoteStatus{}, err
	}
	if err := r.removeExisting(ctx); err != nil {
		return RemoteStatus{}, err
	}
	labels := r.Spec.Labels(revision, material.CAFingerprint, material.NotAfter)
	if _, err := dockerRun(ctx, r.Spec.dockerRunArgs(labels)...); err != nil {
		return RemoteStatus{}, fmt.Errorf("start fetch remote: %w", err)
	}
	if err := r.probe(ctx, material.CAPEM); err != nil {
		return RemoteStatus{}, err
	}
	receipt := remoteReceipt{
		Spec:           r.Spec,
		Revision:       revision,
		ArgoRepository: r.Spec.ArgoRepository(),
		CAFingerprint:  material.CAFingerprint,
		CACertPath:     material.CACertPath,
		CertNotAfter:   material.NotAfter,
		UpdatedAt:      time.Now().UTC(),
	}
	if err := writeReceiptJSON(r.Spec.receiptPath(), receipt); err != nil {
		return RemoteStatus{}, err
	}
	return r.Status(ctx, revision)
}

func (r *FetchRemote) mirror(ctx context.Context, revision string) error {
	if _, err := os.Stat(filepath.Join(r.Spec.RepoDir(), "HEAD")); os.IsNotExist(err) {
		if _, err := gitCommand(ctx, r.Spec.StateDir(), "clone", "--mirror", "--quiet", "--", r.Spec.SourceRepo, r.Spec.RepoDir()); err != nil {
			return fmt.Errorf("mirror source repository: %w", err)
		}
	} else if _, err := gitCommand(ctx, r.Spec.RepoDir(), "remote", "update", "--prune"); err != nil {
		return fmt.Errorf("update mirror: %w", err)
	}
	if _, err := gitCommand(ctx, r.Spec.RepoDir(), "cat-file", "-e", revision+"^{commit}"); err != nil {
		return fmt.Errorf("reviewed revision %s is absent from the mirror: %w", revision, err)
	}
	if _, err := gitCommand(ctx, r.Spec.RepoDir(), "update-ref", remoteServeRef, revision); err != nil {
		return fmt.Errorf("pin reviewed revision: %w", err)
	}
	if _, err := gitCommand(ctx, r.Spec.RepoDir(), "update-server-info"); err != nil {
		return fmt.Errorf("prepare read-only dumb HTTP: %w", err)
	}
	return nil
}

func (r *FetchRemote) removeExisting(ctx context.Context) error {
	state, err := r.inspect(ctx)
	if err != nil {
		return err
	}
	if !state.Exists {
		return nil
	}
	if err = validateOwnership(&r.Spec, &state); err != nil {
		return err
	}
	_, err = dockerRun(ctx, "rm", "--force", r.Spec.ContainerName)
	return err
}

func (r *FetchRemote) probe(ctx context.Context, caPEM []byte) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("fetch remote CA is not usable for verification")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Spec.HostVerifyURL(), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("host verification returned %s", response.Status)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("fetch remote did not become reachable on %s: %w", r.Spec.HostVerifyURL(), lastErr)
}

// Status inspects the container and validates it against the spec and the
// reviewed revision recorded at Up time (unless one is supplied).
func (r *FetchRemote) Status(ctx context.Context, revision string) (RemoteStatus, error) {
	state, err := r.inspect(ctx)
	if err != nil {
		return RemoteStatus{}, err
	}
	status := RemoteStatus{
		Spec:           r.Spec,
		State:          state,
		ArgoRepository: r.Spec.ArgoRepository(),
	}
	if receipt, err := readReceiptJSON(r.Spec.receiptPath()); err == nil {
		status.CACertPath = receipt.CACertPath
		status.CAFingerprint = receipt.CAFingerprint
		if revision == "" {
			revision = receipt.Revision
		}
	}
	status.Revision = revision
	status.Findings = InspectFindings(&r.Spec, &state, revision, time.Now())
	return status, nil
}

// Down validates every ownership and network identity, then removes the
// container. The bare mirror, TLS material, and receipt are preserved so a
// partial creation or deletion is recoverable by re-running Up.
func (r *FetchRemote) Down(ctx context.Context) error {
	state, err := r.inspect(ctx)
	if err != nil {
		return err
	}
	if !state.Exists {
		return nil
	}
	if err = validateOwnership(&r.Spec, &state); err != nil {
		return err
	}
	_, err = dockerRun(ctx, "rm", "--force", r.Spec.ContainerName)
	return err
}

func (r *FetchRemote) inspect(ctx context.Context) (ContainerState, error) {
	out, err := dockerRun(ctx, "inspect", r.Spec.ContainerName)
	if err != nil {
		if strings.Contains(err.Error(), "No such object") || strings.Contains(err.Error(), "no such") {
			return ContainerState{Name: r.Spec.ContainerName}, nil
		}
		return ContainerState{}, err
	}
	return parseContainerState([]byte(out))
}

func dockerRun(ctx context.Context, args ...string) (string, error) {
	return command(ctx, "", "docker", args...)
}

func writeReceiptJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileMode(path, append(data, '\n'), 0o600)
}

func readReceiptJSON(path string) (remoteReceipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return remoteReceipt{}, err
	}
	var receipt remoteReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return remoteReceipt{}, err
	}
	return receipt, nil
}

// AuditFetchRemotes enumerates every container carrying the ownership role and
// audits each one. It never needs a workspace, so `codefly doctor` can call it
// to sweep leaked or drifted remotes across environments.
func AuditFetchRemotes(ctx context.Context, now time.Time) ([]RemoteFinding, int, error) {
	names, err := dockerRun(ctx, "ps", "--all", "--filter", "label="+labelRole+"="+remoteRole, "--format", "{{.Names}}")
	if err != nil {
		return nil, 0, err
	}
	var findings []RemoteFinding
	count := 0
	for name := range strings.FieldsSeq(names) {
		out, err := dockerRun(ctx, "inspect", name)
		if err != nil {
			continue
		}
		state, err := parseContainerState([]byte(out))
		if err != nil {
			continue
		}
		count++
		findings = append(findings, AuditState(&state, now)...)
	}
	return findings, count, nil
}
