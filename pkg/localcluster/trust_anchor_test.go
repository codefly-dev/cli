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

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
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

func installLocalKey(t *testing.T) solutionhost.Signer {
	t.Helper()
	store := &signing.DeveloperKeyStore{Directory: t.TempDir() + "/keys"}
	signer, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	before := localDeveloperKey
	localDeveloperKey = store.Load
	t.Cleanup(func() { localDeveloperKey = before })
	return signer
}

func TestTheVerificationPolicyIsTheHostsLocalProfile(t *testing.T) {
	signer := installLocalKey(t)
	fingerprint := strings.Repeat("ab", 32)
	host := &environments.EnvironmentHost{Domain: "example", Developers: []environments.HostDeveloper{{KeyID: "dev-0123456789abcdef"}, {KeyID: signer.KeyID()}}}
	raw, err := VerificationPolicy(fingerprint, host)
	if err != nil {
		t.Fatal(err)
	}
	var policy verificationPolicy
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&policy); err != nil {
		t.Fatal(err)
	}
	public, _ := signing.PublicKeyPEM(signer)
	if len(policy.Signers) != 1 || policy.Signers[0].KeyID != signer.KeyID() || policy.Signers[0].Name != signer.KeyID() || policy.Signers[0].PublicKey != public || policy.Signers[0].Kind != "person" || len(policy.Signers[0].Domains) != 1 || policy.Signers[0].Domains[0] != "example" || policy.Local.ClusterCertificateAuthoritySha256 != fingerprint {
		t.Fatalf("wrong policy: %s", raw)
	}
	again, _ := VerificationPolicy(fingerprint, host)
	if string(again) != string(raw) {
		t.Fatal("unstable policy")
	}
	host.Developers = host.Developers[:1]
	removed, err := VerificationPolicy(fingerprint, host)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(removed), public) || strings.Contains(string(removed), signer.KeyID()) {
		t.Fatal("removed local key still pinned")
	}
	if AnchorDigest(raw) == AnchorDigest(removed) {
		t.Fatal("removal did not change anchor digest")
	}
	for _, fp := range []string{"abcd", strings.Repeat("AB", 32), strings.Repeat("zz", 32)} {
		if _, err = VerificationPolicy(fp, host); err == nil {
			t.Fatal("invalid fingerprint accepted")
		}
	}
	manifest, err := TrustAnchorManifest("platform", raw)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Kind string
		Data map[string]string
	}
	if err = json.Unmarshal([]byte(manifest), &config); err != nil {
		t.Fatal(err)
	}
	if config.Kind != "ConfigMap" || len(config.Data) != 1 || config.Data[VerificationPolicyFile] != string(raw) {
		t.Fatal("anchor is not exactly one public policy ConfigMap")
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
	signer := installLocalKey(t)
	ca, fingerprint := certificatePEM(t, true)
	cluster.caBundle = string(ca)
	env := &environments.Environment{
		Name: "local", Namespace: "platform",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-dev"},
		Host: &environments.EnvironmentHost{
			Domain: "example", Delivery: "saas/accounts/rest", TrustAnchor: "/etc/host/delivery-trust",
			Developers: []environments.HostDeveloper{{KeyID: signer.KeyID()}},
		},
	}
	driver := &ClusterDriver{
		Workspace: &resources.Workspace{Name: "example"}, Environment: env, Kubectl: cluster.run,
		Poll: time.Millisecond, RolloutTimeout: 20 * time.Millisecond,
	}
	policy, err := VerificationPolicy(fingerprint, env.Host)
	if err != nil {
		t.Fatal(err)
	}
	return driver, fingerprint, AnchorDigest(policy)
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

func TestAnchorReconciliationFollowsOnlyThePolicyConfigMap(t *testing.T) {
	name := gitops.LocalTrustAnchorObjects
	for label, spec := range map[string]map[string]any{
		"volume":    {"volumes": []any{map[string]any{"configMap": map[string]any{"name": name}}}},
		"projected": {"volumes": anchorVolume("trust")},
		"envFrom":   {"containers": []any{map[string]any{"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": name}}}}}},
		"initEnv":   {"initContainers": []any{map[string]any{"env": []any{map[string]any{"valueFrom": map[string]any{"configMapKeyRef": map[string]any{"name": name}}}}}}},
	} {
		if !consumesAnchor(spec) {
			t.Fatalf("%s did not consume the policy", label)
		}
	}
	if consumesAnchor(map[string]any{"volumes": []any{map[string]any{"secret": map[string]any{"secretName": name}}}}) {
		t.Fatal("an unrelated Secret is treated as the policy")
	}
}

// A native run's host reads a local profile the run writes itself: bound to
// the run's workload-identity anchor, never to a cluster, and admitting the run's own delivery key for the host's domain,
// with no developer listed in the workspace. No key, or no host, is refused.
func TestANativeHostPolicyAdmitsTheRunsOwnSignerWithoutDevelopers(t *testing.T) {
	signer := installLocalKey(t)
	fingerprint := strings.Repeat("cd", 32)
	host := &environments.EnvironmentHost{Domain: "obin"}
	raw, err := NativeVerificationPolicy(fingerprint, signer, host)
	if err != nil {
		t.Fatal(err)
	}
	var policy verificationPolicy
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&policy); err != nil {
		t.Fatal(err)
	}
	public, _ := signing.PublicKeyPEM(signer)
	if policy.Local.NativeAnchorSha256 != fingerprint || policy.Local.ClusterCertificateAuthoritySha256 != "" || len(policy.Signers) != 1 || policy.Signers[0].KeyID != signer.KeyID() || policy.Signers[0].PublicKey != public || policy.Signers[0].Kind != "person" || len(policy.Signers[0].Domains) != 1 || policy.Signers[0].Domains[0] != "obin" {
		t.Fatalf("wrong native policy: %s", raw)
	}
	if _, err = NativeVerificationPolicy(fingerprint, nil, host); err == nil {
		t.Fatal("no delivery key was admitted")
	}
	if _, err = NativeVerificationPolicy(fingerprint, signer, nil); err == nil {
		t.Fatal("no host was accepted")
	}
	if _, err = NativeVerificationPolicy("abcd", signer, host); err == nil {
		t.Fatal("a malformed fingerprint was accepted")
	}
}
