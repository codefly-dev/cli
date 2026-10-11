package localcluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
)

func certificatePEM(t *testing.T, isCA bool) ([]byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "k3s-server-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(digest[:])
}

// TestTheLocalProfileIsBoundToTheClustersAuthority: the fingerprint is the
// SHA-256 of the DER of the one CA the cluster projects — the value the host
// proves at boot — and a bundle that cannot say which CA the API server
// serves under is refused rather than guessed.
func TestTheLocalProfileIsBoundToTheClustersAuthority(t *testing.T) {
	ca, want := certificatePEM(t, true)
	got, err := ClusterCAFingerprint(ca)
	if err != nil || got != want {
		t.Fatalf("fingerprint %s (%v), want %s", got, err, want)
	}
	other, _ := certificatePEM(t, true)
	leaf, _ := certificatePEM(t, false)
	for name, bundle := range map[string][]byte{"no certificate": []byte("not pem"), "two authorities": append(append([]byte{}, ca...), other...), "not an authority": leaf} {
		if _, err := ClusterCAFingerprint(bundle); err == nil {
			t.Errorf("%s: a fingerprint was taken", name)
		}
	}
}

// TestTheVerificationPolicyIsTheHostsLocalProfile pins the document to the
// host's schema (module-saas-starter#1066): the local block with the cluster
// fingerprint, one person signer per listed developer with the exact issuer
// and address, each speaking for the environment's ownership domain.
func TestTheVerificationPolicyIsTheHostsLocalProfile(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	host := &environments.EnvironmentHost{Domain: "example", Developers: []environments.HostDeveloper{
		{Issuer: "https://github.com/login/oauth", Subject: "zed@example.test"},
		{Issuer: "https://accounts.google.com", Subject: "ann@example.test"},
	}}
	raw, err := VerificationPolicy(fingerprint, host)
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err = json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy) != 2 || policy["local"].(map[string]any)["clusterCertificateAuthoritySha256"] != fingerprint {
		t.Fatalf("policy %s does not bind the cluster", raw)
	}
	signers := policy["signers"].([]any)
	first := signers[0].(map[string]any)
	if len(signers) != 2 || len(first) != 5 || first["kind"] != "person" || first["issuer"] != "https://accounts.google.com" ||
		first["subjectAlternativeName"] != "ann@example.test" || first["name"] != "ann@example.test" ||
		len(first["domains"].([]any)) != 1 || first["domains"].([]any)[0] != "example" {
		t.Fatalf("signers %s are not the listed developers as persons of the environment's domain", raw)
	}
	for name, fp := range map[string]string{"short": "abcd", "upper": strings.Repeat("AB", 32)} {
		if _, err = VerificationPolicy(fp, host); err == nil {
			t.Errorf("%s fingerprint accepted", name)
		}
	}
	if _, err = VerificationPolicy(fingerprint, &environments.EnvironmentHost{Domain: "example"}); err == nil {
		t.Error("a profile admitting nobody was written")
	}
}

// clusterState is a fake namespace's workloads: their kind, the volumes their
// pods mount and their pod-template annotation, as a reconciliation reads and
// patches them.
type clusterState struct {
	caBundle   string
	workloads  []fakeWorkload
	failPatch  int
	failStatus int
	calls      []string
	stdins     []string
}

type fakeWorkload struct {
	kind, name string
	volumes    []string
	anchor     string
}

func (c *clusterState) run(_ context.Context, stdin string, args ...string) (string, error) {
	call := strings.Join(args, " ")
	c.calls = append(c.calls, call)
	c.stdins = append(c.stdins, stdin)
	switch {
	case strings.Contains(call, "kube-root-ca.crt"):
		return c.caBundle, nil
	case strings.Contains(call, "get deployments,statefulsets,daemonsets"):
		var items []any
		for _, workload := range c.workloads {
			volumes := []any{}
			for _, volume := range workload.volumes {
				volumes = append(volumes, map[string]any{"name": volume})
			}
			items = append(items, map[string]any{
				"kind": workload.kind, "metadata": map[string]any{"name": workload.name},
				"spec": map[string]any{"template": map[string]any{
					"metadata": map[string]any{"annotations": map[string]string{TrustAnchorAnnotation: workload.anchor}},
					"spec":     map[string]any{"volumes": volumes},
				}},
			})
		}
		encoded, err := json.Marshal(map[string]any{"items": items})
		return string(encoded), err
	case strings.Contains(call, " patch "):
		if c.failPatch > 0 {
			c.failPatch--
			return "", errors.New("the API server is unavailable")
		}
		var patch struct {
			Spec struct {
				Template struct {
					Metadata struct {
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		}
		_ = json.Unmarshal([]byte(args[len(args)-1]), &patch)
		for index := range c.workloads {
			if strings.Contains(call, " "+strings.ToLower(c.workloads[index].kind)+"/"+c.workloads[index].name+" ") {
				c.workloads[index].anchor = patch.Spec.Template.Metadata.Annotations[TrustAnchorAnnotation]
			}
		}
	case strings.Contains(call, "rollout status"):
		if c.failStatus > 0 {
			c.failStatus--
			return "", errors.New("timed out waiting for the rollout")
		}
	}
	return "", nil
}

func (c *clusterState) count(fragment string) int {
	count := 0
	for _, call := range c.calls {
		if strings.Contains(call, fragment) {
			count++
		}
	}
	return count
}

// TestTheTrustAnchorIsInForceOnlyOnceTheVerifierRestartedOnIt is the attack
// the review named: a developer is REMOVED, the anchor is written, and the
// restart fails. Convergence is decided by the workloads' own state — the
// anchor's digest stamped on each mounting pod template, every Deployment,
// StatefulSet and DaemonSet, and each rollout awaited — so the next run finds
// the verifier behind the digest and stamps it again, and a rollout that did
// not complete is awaited again; a workload that does not mount the anchor is
// never touched.
func TestTheTrustAnchorIsInForceOnlyOnceTheVerifierRestartedOnIt(t *testing.T) {
	ca, fingerprint := certificatePEM(t, true)
	env := &environments.Environment{
		Name: "local", Namespace: "platform",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-dev"},
		Host: &environments.EnvironmentHost{
			Domain: "example", Delivery: "saas/accounts/rest", TrustAnchor: "/etc/host/delivery-trust",
			Developers: []environments.HostDeveloper{{Issuer: "https://github.com/login/oauth", Subject: "remaining@example.test"}},
		},
	}
	cluster := &clusterState{caBundle: string(ca), failPatch: 1, workloads: []fakeWorkload{
		{kind: "Deployment", name: "accounts", volumes: []string{gitops.LocalTrustAnchorVolume}, anchor: "sha256:the-policy-that-admitted-the-removed-developer"},
		{kind: "DaemonSet", name: "accounts-edge", volumes: []string{gitops.LocalTrustAnchorVolume}},
		{kind: "Deployment", name: "gateway", volumes: []string{"codefly-workload-identity"}},
	}}
	root := []byte(`{"mediaType":"root"}`)
	driver := &ClusterDriver{
		Workspace: &resources.Workspace{Name: "example"}, Environment: env, Kubectl: cluster.run,
		TrustedRoot: func(context.Context) ([]byte, error) { return root, nil },
	}
	policy, err := VerificationPolicy(fingerprint, env.Host)
	if err != nil {
		t.Fatal(err)
	}
	digest := AnchorDigest(policy, root)

	if _, err = driver.ProvisionTrustAnchor(context.Background()); err == nil || !strings.Contains(err.Error(), "next run retries") {
		t.Fatalf("a failed restart was reported as provisioned: %v", err)
	}
	if cluster.workloads[0].anchor == digest {
		t.Fatal("the fake did not fail the stamp")
	}
	got, err := driver.ProvisionTrustAnchor(context.Background())
	if err != nil || got != fingerprint {
		t.Fatalf("the retry: %s, %v", got, err)
	}
	for _, workload := range cluster.workloads[:2] {
		if workload.anchor != digest {
			t.Fatalf("%s/%s runs on anchor %q after the retry, want %s", workload.kind, workload.name, workload.anchor, digest)
		}
	}
	if cluster.workloads[2].anchor != "" || cluster.count("deployment/gateway") != 0 {
		t.Fatal("a workload that does not mount the anchor was stamped or awaited")
	}
	if cluster.count("rollout status deployment/accounts") == 0 || cluster.count("rollout status daemonset/accounts-edge") == 0 {
		t.Fatalf("the rollouts were not awaited: %v", cluster.calls)
	}

	// Stamped but not rolled out: the next run patches nothing and waits again.
	cluster.calls, cluster.failStatus = nil, 1
	if _, err = driver.ProvisionTrustAnchor(context.Background()); err == nil || !strings.Contains(err.Error(), "waits again") {
		t.Fatalf("an incomplete rollout was reported as in force: %v", err)
	}
	cluster.calls = nil
	if _, err = driver.ProvisionTrustAnchor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.count(" patch ") != 0 || cluster.count("rollout status") != 2 {
		t.Fatalf("a converged anchor was re-stamped or not re-awaited: %v", cluster.calls)
	}
	var manifest string
	for index, call := range cluster.calls {
		if strings.HasPrefix(call, "apply") {
			manifest = cluster.stdins[index]
		}
	}
	for _, want := range []string{`"kind":"ConfigMap"`, `"kind":"Secret"`, `"name":"` + gitops.LocalTrustAnchorObjects + `"`, `"namespace":"platform"`, VerificationPolicyFile, TrustedRootFile, fingerprint} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("the trust anchor manifest lacks %s:\n%s", want, manifest)
		}
	}
}
