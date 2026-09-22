package deployments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
)

// KubernetesTargetBinding contains no credentials or local configuration paths.
// Kubernetes has no cluster UID: SystemNamespaceUID identifies the observed
// kube-system incarnation, alongside the verified cluster routing/CA identity.
// This observation is neither mutation authority nor an effect fence.
type KubernetesTargetBinding struct {
	Schema             string `json:"schema"`
	ClusterIdentity    string `json:"clusterIdentity"`
	SystemNamespaceUID string `json:"systemNamespaceUID"`
	Namespace          string `json:"namespace"`
	NamespaceUID       string `json:"namespaceUID"`
}

type KubernetesTargetInspection struct {
	Identity   string                  `json:"identity"`
	Binding    KubernetesTargetBinding `json:"binding"`
	ObservedAt time.Time               `json:"observedAt"`
}

var targetNamespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// InspectLocalKubernetesTarget reads the real API through a verified kubeconfig
// snapshot. Namespace is explicit, not a fallback to the context's default.
func InspectLocalKubernetesTarget(ctx context.Context, env *environments.Environment) (*KubernetesTargetInspection, error) {
	if env == nil || len(env.Namespace) > 63 || !targetNamespaceName.MatchString(env.Namespace) {
		return nil, errors.New("target inspection requires an explicit valid environment namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, snapshot, err := verifyLocalK3dTarget(ctx, env)
	if err != nil {
		return nil, err
	}
	if err = validateTargetTLS(snapshot, target.Cluster); err != nil {
		return nil, err
	}
	systemUID, err := readActiveNamespaceUID(ctx, &target, snapshot, "kube-system")
	if err != nil {
		return nil, err
	}
	namespaceUID := systemUID
	if env.Namespace != "kube-system" {
		namespaceUID, err = readActiveNamespaceUID(ctx, &target, snapshot, env.Namespace)
		if err != nil {
			return nil, err
		}
	}
	if err = VerifyLocalK3dTargetUnchanged(ctx, env, &target); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	binding := KubernetesTargetBinding{Schema: "codefly/kubernetes-target/v1", ClusterIdentity: target.ClusterIdentity,
		SystemNamespaceUID: systemUID, Namespace: env.Namespace, NamespaceUID: namespaceUID}
	data, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	return &KubernetesTargetInspection{Identity: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Binding: binding, ObservedAt: time.Now().UTC()}, nil
}

// RecheckLocalKubernetesTarget takes the independently retained binding digest,
// not a candidate's assertions about where its outputs may be deployed.
func RecheckLocalKubernetesTarget(ctx context.Context, env *environments.Environment, expected string) (*KubernetesTargetInspection, error) {
	hexDigest, ok := strings.CutPrefix(expected, "sha256:")
	digest, err := hex.DecodeString(hexDigest)
	if !ok || err != nil || len(digest) != sha256.Size || hexDigest != strings.ToLower(hexDigest) {
		return nil, errors.New("target recheck requires an independently retained canonical SHA-256 identity")
	}
	observed, err := InspectLocalKubernetesTarget(ctx, env)
	if err != nil {
		return nil, err
	}
	if observed.Identity != expected {
		return nil, errors.New("live Kubernetes target differs from the retained binding; requalification and approval are required")
	}
	return observed, nil
}

func validateTargetTLS(snapshot []byte, name string) error {
	var config kubeconfigView
	if err := json.Unmarshal(snapshot, &config); err != nil {
		return errors.New("cannot decode verified target configuration")
	}
	for _, cluster := range config.Clusters {
		if cluster.Name != name {
			continue
		}
		server, _ := cluster.Cluster["server"].(string)
		endpoint, err := url.Parse(server)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
			return errors.New("target identity requires an authenticated HTTPS API server")
		}
		if value, exists := cluster.Cluster["insecure-skip-tls-verify"]; exists {
			insecure, valid := value.(bool)
			if !valid || insecure {
				return errors.New("target identity cannot skip API server TLS verification")
			}
		}
		encoded, _ := cluster.Cluster["certificate-authority-data"].(string)
		certificates, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || !x509.NewCertPool().AppendCertsFromPEM(certificates) {
			return errors.New("target identity requires the resolved cluster certificate authority")
		}
		return nil
	}
	return errors.New("verified target cluster is absent from its configuration snapshot")
}

func readActiveNamespaceUID(ctx context.Context, target *VerifiedKubernetesTarget, snapshot []byte, name string) (string, error) {
	if len(name) > 63 || !targetNamespaceName.MatchString(name) {
		return "", errors.New("namespace query requires a canonical namespace name")
	}
	// This read-only invocation needs no manifest stdin. Use the verified
	// flattened config directly, without persisting credentials in a temp file.
	args := []string{"--kubeconfig", "/dev/stdin", "--context", target.Context, "get", "namespace", name, "-o", "json"}
	command := exec.CommandContext(ctx, "kubectl", args...)
	command.Stdin = bytes.NewReader(snapshot)
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("inspect target namespace %q: %w", name, errors.Join(err, ctx.Err()))
	}
	var namespace struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name              string  `json:"name"`
			UID               string  `json:"uid"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(output.Bytes(), &namespace); err != nil {
		return "", errors.New("cannot decode target namespace identity")
	}
	if namespace.APIVersion != "v1" || namespace.Kind != "Namespace" || namespace.Metadata.Name != name ||
		namespace.Metadata.UID == "" || namespace.Metadata.UID != strings.TrimSpace(namespace.Metadata.UID) ||
		namespace.Metadata.DeletionTimestamp != nil || namespace.Status.Phase != "Active" {
		return "", fmt.Errorf("target namespace %q must have an active, non-deleting identity", name)
	}
	return namespace.Metadata.UID, nil
}
