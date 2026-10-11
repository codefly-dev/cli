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
	"fmt"
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

// clusterState is a fake namespace's workloads and their pods, as a
// reconciliation reads, patches and waits on them. A stamped template rolls
// its pods onto the new annotation unless the update strategy keeps them.
type clusterState struct {
	caBundle   string
	workloads  []*fakeWorkload
	failPatch  int
	failStatus int
	revert     bool // a competing rollout puts the old template back after the stamp
	calls      []string
	stdins     []string
}

type fakeWorkload struct {
	kind, name string
	volumes    []any // pod volumes, as manifests carry them
	anchor     string
	pods       []string // the anchor each live pod was started on
	onDelete   bool
	partition  int
}

func anchorVolume(name string) []any {
	return []any{map[string]any{"name": name, "projected": map[string]any{"sources": []any{
		map[string]any{"configMap": map[string]any{"name": gitops.LocalTrustAnchorObjects}},
		map[string]any{"secret": map[string]any{"name": gitops.LocalTrustAnchorObjects}},
	}}}}
}

func (workload *fakeWorkload) manifest() map[string]any {
	strategy := map[string]any{}
	if workload.onDelete {
		strategy["type"] = "OnDelete"
	}
	if workload.partition > 0 {
		strategy["rollingUpdate"] = map[string]any{"partition": workload.partition}
	}
	return map[string]any{
		"kind": workload.kind, "metadata": map[string]any{"name": workload.name},
		"spec": map[string]any{
			"selector":       map[string]any{"matchLabels": map[string]string{"app": workload.name}},
			"updateStrategy": strategy,
			"template": map[string]any{
				"metadata": map[string]any{"annotations": map[string]string{TrustAnchorAnnotation: workload.anchor}},
				"spec":     map[string]any{"volumes": workload.volumes},
			},
		},
	}
}

func (c *clusterState) find(call string) *fakeWorkload {
	for _, workload := range c.workloads {
		if strings.Contains(call, " "+strings.ToLower(workload.kind)+"/"+workload.name+" ") || strings.Contains(call, " app="+workload.name+" ") {
			return workload
		}
	}
	return nil
}

func (c *clusterState) run(_ context.Context, stdin string, args ...string) (string, error) {
	call := strings.Join(args, " ") + " "
	c.calls = append(c.calls, call)
	c.stdins = append(c.stdins, stdin)
	switch {
	case strings.Contains(call, "kube-root-ca.crt"):
		return c.caBundle, nil
	case strings.Contains(call, "get deployments,statefulsets,daemonsets"):
		items := []any{}
		for _, workload := range c.workloads {
			items = append(items, workload.manifest())
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
		workload := c.find(call)
		previous := workload.anchor
		workload.anchor = patch.Spec.Template.Metadata.Annotations[TrustAnchorAnnotation]
		if !workload.onDelete {
			for index := range workload.pods {
				if index >= workload.partition {
					workload.pods[index] = workload.anchor
				}
			}
		}
		if c.revert {
			workload.anchor = previous
		}
	case strings.Contains(call, "rollout status"):
		if c.failStatus > 0 {
			c.failStatus--
			return "", errors.New("timed out waiting for the rollout")
		}
	case strings.Contains(call, "get pods"):
		workload := c.find(call)
		items := []any{}
		for index, anchor := range workload.pods {
			items = append(items, map[string]any{"metadata": map[string]any{
				"name": fmt.Sprintf("%s-%d", workload.name, index), "annotations": map[string]string{TrustAnchorAnnotation: anchor},
			}})
		}
		encoded, err := json.Marshal(map[string]any{"items": items})
		return string(encoded), err
	case strings.Contains(call, " get "):
		encoded, err := json.Marshal(c.find(call).manifest())
		return string(encoded), err
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

func anchorDriver(t *testing.T, cluster *clusterState) (*ClusterDriver, string, string) {
	t.Helper()
	ca, fingerprint := certificatePEM(t, true)
	cluster.caBundle = string(ca)
	env := &environments.Environment{
		Name: "local", Namespace: "platform",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-dev"},
		Host: &environments.EnvironmentHost{
			Domain: "example", Delivery: "saas/accounts/rest", TrustAnchor: "/etc/host/delivery-trust",
			Developers: []environments.HostDeveloper{{Issuer: "https://github.com/login/oauth", Subject: "remaining@example.test"}},
		},
	}
	root := []byte(`{"mediaType":"root"}`)
	driver := &ClusterDriver{
		Workspace: &resources.Workspace{Name: "example"}, Environment: env, Kubectl: cluster.run,
		TrustedRoot: func(context.Context) ([]byte, error) { return root, nil },
		Poll:        time.Millisecond, RolloutTimeout: 20 * time.Millisecond,
	}
	policy, err := VerificationPolicy(fingerprint, env.Host)
	if err != nil {
		t.Fatal(err)
	}
	return driver, fingerprint, AnchorDigest(policy, root)
}

const removedDeveloper = "sha256:the-policy-that-admitted-the-removed-developer"

// TestTheTrustAnchorIsInForceOnlyOnceEveryPodRunsIt is the revocation
// attack, round 1 and round 2: a developer is REMOVED and the anchor
// re-written. Every Deployment, StatefulSet and DaemonSet that reads either
// anchor object — whatever its volume is named — is stamped with the
// anchor's digest, its rollout awaited, its template read back, and every
// live pod must carry the digest. A failed stamp is retried by the next run;
// an incomplete rollout is waited on again; a workload that reads nothing of
// the anchor is never touched.
func TestTheTrustAnchorIsInForceOnlyOnceEveryPodRunsIt(t *testing.T) {
	cluster := &clusterState{failPatch: 1, workloads: []*fakeWorkload{
		{kind: "Deployment", name: "accounts", volumes: anchorVolume(gitops.LocalTrustAnchorVolume), anchor: removedDeveloper, pods: []string{removedDeveloper, removedDeveloper}},
		{kind: "Deployment", name: "accounts-replica", volumes: anchorVolume("host-trust"), anchor: removedDeveloper, pods: []string{removedDeveloper}},
		{kind: "DaemonSet", name: "accounts-edge", volumes: []any{map[string]any{"name": "policy", "configMap": map[string]any{"name": gitops.LocalTrustAnchorObjects}}}, pods: []string{""}},
		{kind: "Deployment", name: "gateway", volumes: []any{map[string]any{"name": "other", "emptyDir": map[string]any{}}}, pods: []string{""}},
	}}
	driver, fingerprint, digest := anchorDriver(t, cluster)

	if _, err := driver.ProvisionTrustAnchor(context.Background()); err == nil || !strings.Contains(err.Error(), "next run retries") {
		t.Fatalf("a failed restart was reported as provisioned: %v", err)
	}
	got, err := driver.ProvisionTrustAnchor(context.Background())
	if err != nil || got != fingerprint {
		t.Fatalf("the retry: %s, %v", got, err)
	}
	for _, workload := range cluster.workloads[:3] {
		if workload.anchor != digest {
			t.Fatalf("%s/%s carries %q, want %s", workload.kind, workload.name, workload.anchor, digest)
		}
		for index, pod := range workload.pods {
			if pod != digest {
				t.Fatalf("pod %d of %s still runs %q", index, workload.name, pod)
			}
		}
	}
	if cluster.workloads[3].anchor != "" || cluster.count("deployment/gateway") != 0 {
		t.Fatal("a workload that reads nothing of the anchor was stamped or awaited")
	}

	cluster.calls, cluster.failStatus = nil, 1
	if _, err = driver.ProvisionTrustAnchor(context.Background()); err == nil || !strings.Contains(err.Error(), "waits again") {
		t.Fatalf("an incomplete rollout was reported as in force: %v", err)
	}
	cluster.calls = nil
	if _, err = driver.ProvisionTrustAnchor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.count(" patch ") != 0 {
		t.Fatalf("a converged anchor was re-stamped: %v", cluster.calls)
	}
}

// TestARevocationThatCannotReachEveryPodIsNeverReportedDone: a StatefulSet
// partitioned past its pods and an OnDelete DaemonSet accept the stamp and
// keep every pod on the old anchor; a competing rollout puts the old template
// back. Each is refused — never reported as in force.
func TestARevocationThatCannotReachEveryPodIsNeverReportedDone(t *testing.T) {
	for name, workload := range map[string]*fakeWorkload{
		"a partitioned StatefulSet": {kind: "StatefulSet", name: "accounts", volumes: anchorVolume(gitops.LocalTrustAnchorVolume), anchor: removedDeveloper, pods: []string{removedDeveloper, removedDeveloper, removedDeveloper}, partition: 3},
		"an OnDelete DaemonSet":     {kind: "DaemonSet", name: "accounts", volumes: anchorVolume(gitops.LocalTrustAnchorVolume), anchor: removedDeveloper, pods: []string{removedDeveloper}, onDelete: true},
	} {
		driver, _, _ := anchorDriver(t, &clusterState{workloads: []*fakeWorkload{workload}})
		if _, err := driver.ProvisionTrustAnchor(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot prove") {
			t.Errorf("%s: a revocation that reached no pod was reported: %v", name, err)
		}
	}
	reverted := &clusterState{revert: true, workloads: []*fakeWorkload{
		{kind: "Deployment", name: "accounts", volumes: anchorVolume(gitops.LocalTrustAnchorVolume), anchor: removedDeveloper, pods: []string{removedDeveloper}},
	}}
	driver, _, _ := anchorDriver(t, reverted)
	if _, err := driver.ProvisionTrustAnchor(context.Background()); err == nil || !strings.Contains(err.Error(), "moved off") {
		t.Errorf("a stamp another rollout reverted was reported as in force: %v", err)
	}
}
