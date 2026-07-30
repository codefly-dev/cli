package gitops

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

const testImage = "nginx:1.27.3-alpine@sha256:41523187cf7d7a2f2677a80609d9caa14388bf5c1fbca9c410ba3de602aaaab4"

func testConfig() RemoteConfig {
	return RemoteConfig{
		WorkspaceRoot:  "/tmp/workspace",
		Owner:          "Ada",
		Workspace:      "payments-ws",
		Environment:    "local",
		Cluster:        "codefly-local",
		RepositorySlug: "codefly-test/manifests",
		SourceRepo:     "file:///tmp/remote.git",
		Image:          testImage,
	}
}

func mustSpec(t *testing.T, cfg RemoteConfig) RemoteSpec {
	t.Helper()
	spec, err := NewRemoteSpec(&cfg)
	if err != nil {
		t.Fatalf("NewRemoteSpec: %v", err)
	}
	return spec
}

func TestNewRemoteSpecRejectsUnsafeInput(t *testing.T) {
	cases := map[string]func(*RemoteConfig){
		"empty root":      func(c *RemoteConfig) { c.WorkspaceRoot = "" },
		"empty owner":     func(c *RemoteConfig) { c.Owner = "" },
		"empty cluster":   func(c *RemoteConfig) { c.Cluster = "" },
		"empty slug":      func(c *RemoteConfig) { c.RepositorySlug = "" },
		"empty source":    func(c *RemoteConfig) { c.SourceRepo = "" },
		"mutable image":   func(c *RemoteConfig) { c.Image = "nginx:latest" },
		"non-loopback":    func(c *RemoteConfig) { c.HostAddr = "0.0.0.0" },
		"public host":     func(c *RemoteConfig) { c.HostAddr = "10.0.0.1" },
		"rotate too long": func(c *RemoteConfig) { c.CertValidity = time.Hour; c.CertRotateBefore = 2 * time.Hour },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			if _, err := NewRemoteSpec(&cfg); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestNewRemoteSpecDeterministicIdentity(t *testing.T) {
	first := mustSpec(t, testConfig())
	second := mustSpec(t, testConfig())
	if first.ContainerName != second.ContainerName {
		t.Fatalf("container name not deterministic: %s vs %s", first.ContainerName, second.ContainerName)
	}
	if first.ContainerName != first.DNSName {
		t.Fatalf("dns name must equal container name, got %s and %s", first.ContainerName, first.DNSName)
	}
	// A zero host port means Docker binds a free loopback port at start time.
	if first.HostPort != 0 {
		t.Fatalf("host port should default to Docker-assigned (0), got %d", first.HostPort)
	}
	if strings.ContainsAny(first.ContainerName, " _/:") || first.ContainerName != strings.ToLower(first.ContainerName) {
		t.Fatalf("container name is not a DNS label: %q", first.ContainerName)
	}
	if got := first.ArgoRepository(); got != "https://"+first.DNSName+"/repo.git" {
		t.Fatalf("argo repository = %q", got)
	}
	// A different repository slug must yield a different identity.
	other := testConfig()
	other.RepositorySlug = "codefly-test/other"
	if mustSpec(t, other).ContainerName == first.ContainerName {
		t.Fatalf("identity did not change with repository slug")
	}
}

// The container name must NOT depend on the human owner: a transient change in
// $USER between `up` and `down` would otherwise compute a different name and
// make teardown silently miss the running container.
func TestNewRemoteSpecIdentityIndependentOfOwner(t *testing.T) {
	ada := mustSpec(t, testConfig())
	other := testConfig()
	other.Owner = "grace"
	grace := mustSpec(t, other)
	if ada.ContainerName != grace.ContainerName {
		t.Fatalf("container name changed with owner: %s vs %s", ada.ContainerName, grace.ContainerName)
	}
	if ada.Owner == grace.Owner {
		t.Fatalf("owner should still be recorded distinctly, both are %q", ada.Owner)
	}
}

func TestDockerRunArgsEncodeInvariants(t *testing.T) {
	spec := mustSpec(t, testConfig())
	labels := spec.Labels("a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", "sha256:deadbeef", time.Unix(0, 0))
	args := spec.dockerRunArgs(labels)
	joined := strings.Join(args, " ")

	if !slices.Contains(args, "--read-only") {
		t.Fatalf("missing --read-only in %v", args)
	}
	if args[len(args)-1] != spec.Image || !strings.Contains(spec.Image, "@sha256:") {
		t.Fatalf("image must be the final digest-pinned argument, got %q", args[len(args)-1])
	}
	// Default (HostPort 0): loopback host, Docker-assigned port, never a wildcard.
	if !strings.Contains(joined, "--publish 127.0.0.1::443") {
		t.Fatalf("default binding is not loopback + Docker-assigned: %q", joined)
	}
	if strings.Contains(joined, "0.0.0.0") || strings.Contains(joined, "--publish :") {
		t.Fatalf("host binding leaks a wildcard: %q", joined)
	}
	if !strings.Contains(joined, "--network "+spec.Network) {
		t.Fatalf("missing private network: %q", joined)
	}
	// nginx runs on a read-only rootfs; every path it writes must be tmpfs,
	// including its log directory (opened before our config is read).
	if !strings.Contains(joined, "--tmpfs /var/log/nginx") {
		t.Fatalf("missing writable log tmpfs (nginx would fail to start read-only): %q", joined)
	}
	for _, mount := range []string{
		spec.RepoDir() + ":" + remoteMountRepo + ":ro",
		spec.TLSDir() + ":" + remoteMountTLS + ":ro",
	} {
		if !strings.Contains(joined, mount) {
			t.Fatalf("missing read-only mount %q in %q", mount, joined)
		}
	}
	if !strings.Contains(joined, "--label "+labelRole+"="+remoteRole) {
		t.Fatalf("missing ownership role label: %q", joined)
	}

	// An explicit host port is published verbatim (still loopback-bound).
	fixed := testConfig()
	fixed.HostPort = 55555
	fixedSpec := mustSpec(t, fixed)
	if got := strings.Join(fixedSpec.dockerRunArgs(labels), " "); !strings.Contains(got, "--publish 127.0.0.1:55555:443") {
		t.Fatalf("explicit host port not published: %q", got)
	}
}

func TestNginxConfigDoesNotWriteToReadonlyPaths(t *testing.T) {
	spec := mustSpec(t, testConfig())
	config := spec.nginxConfig()
	// error_log must go to stderr, not a file under the read-only rootfs.
	if !strings.Contains(config, "error_log stderr") {
		t.Fatalf("nginx error_log is not directed to stderr: %s", config)
	}
	if !strings.Contains(config, "access_log off") {
		t.Fatalf("nginx access_log is not disabled: %s", config)
	}
}

func TestLoopbackHostPort(t *testing.T) {
	loopback := ContainerState{
		Name:         "remote",
		PortBindings: []PortBinding{{ContainerPort: "443/tcp", HostIP: "127.0.0.1", HostPort: "49221"}},
	}
	port, err := loopbackHostPort(&loopback, 443)
	if err != nil || port != "49221" {
		t.Fatalf("loopbackHostPort = %q, %v", port, err)
	}

	wildcard := ContainerState{
		Name:         "remote",
		PortBindings: []PortBinding{{ContainerPort: "443/tcp", HostIP: "0.0.0.0", HostPort: "49221"}},
	}
	if _, err := loopbackHostPort(&wildcard, 443); err == nil {
		t.Fatalf("expected wildcard binding to be rejected")
	}

	missing := ContainerState{Name: "remote"}
	if _, err := loopbackHostPort(&missing, 443); err == nil {
		t.Fatalf("expected missing binding to be rejected")
	}
}

func TestRefsAdvertisesRevision(t *testing.T) {
	revision := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	advertised := []byte("a1b2c3d4e5f60718293a4b5c6d7e8f9012345678\trefs/heads/codefly-fetch\n" +
		"0000000000000000000000000000000000000000\trefs/heads/main\n")
	if !refsAdvertisesRevision(advertised, revision) {
		t.Fatalf("expected the reviewed revision to be recognized as advertised")
	}
	// A static file that is served but does not list the reviewed revision must
	// not count as a healthy serve.
	if refsAdvertisesRevision([]byte("<html>not a git advertisement</html>"), revision) {
		t.Fatalf("non-advertising body wrongly accepted")
	}
	if refsAdvertisesRevision(nil, revision) {
		t.Fatalf("empty body wrongly accepted")
	}
}

func TestEnsureTLSMaterialPermissionsAndSANs(t *testing.T) {
	spec := mustSpec(t, testConfig())
	dir := t.TempDir()
	now := time.Now()
	material, err := ensureTLSMaterial(dir, &spec, now)
	if err != nil {
		t.Fatalf("ensureTLSMaterial: %v", err)
	}
	if !material.Rotated {
		t.Fatalf("first generation must report a rotation")
	}
	for _, key := range []string{"ca.key", "server.key"} {
		info, err := os.Stat(filepath.Join(dir, key))
		if err != nil {
			t.Fatalf("stat %s: %v", key, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", key, info.Mode().Perm())
		}
	}

	serverPEM, err := os.ReadFile(filepath.Join(dir, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(serverPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != spec.DNSName {
		t.Fatalf("server DNS SANs = %v, want [%s]", cert.DNSNames, spec.DNSName)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != spec.HostAddr {
		t.Fatalf("server IP SANs = %v, want [%s]", cert.IPAddresses, spec.HostAddr)
	}
	if cert.NotAfter.Sub(now) > spec.CertValidity+time.Minute {
		t.Fatalf("certificate validity unbounded: NotAfter=%s", cert.NotAfter)
	}

	// The CA PEM must be a usable trust anchor and match the reported fingerprint.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(material.CAPEM) {
		t.Fatalf("CA PEM is not a usable trust anchor")
	}
	if !strings.HasPrefix(material.CAFingerprint, "sha256:") {
		t.Fatalf("CA fingerprint = %q", material.CAFingerprint)
	}
}

func TestEnsureTLSMaterialReuseAndRotation(t *testing.T) {
	spec := mustSpec(t, testConfig())
	dir := t.TempDir()
	now := time.Now()
	first, err := ensureTLSMaterial(dir, &spec, now)
	if err != nil {
		t.Fatal(err)
	}
	// A second call within validity reuses the material.
	reused, err := ensureTLSMaterial(dir, &spec, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if reused.Rotated {
		t.Fatalf("expected reuse, got rotation")
	}
	if reused.CAFingerprint != first.CAFingerprint {
		t.Fatalf("reuse changed the CA fingerprint")
	}
	// Inside the rotation window it must mint fresh material.
	rotated, err := ensureTLSMaterial(dir, &spec, now.Add(spec.CertValidity-spec.CertRotateBefore+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.Rotated {
		t.Fatalf("expected rotation inside the window")
	}
	if rotated.CAFingerprint == first.CAFingerprint {
		t.Fatalf("rotation reused the old CA")
	}
}

func TestParseContainerState(t *testing.T) {
	data := []byte(`[{
		"Name": "/codefly-gitops-remote-local-abcd1234",
		"State": {"Running": true},
		"Config": {"Image": "nginx@sha256:aa", "Labels": {"codefly.gitops.role": "gitops-fetch-remote"}},
		"NetworkSettings": {"Networks": {"k3d-codefly-local": {}}},
		"HostConfig": {"PortBindings": {"443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "50000"}]}}
	}]`)
	state, err := parseContainerState(data)
	if err != nil {
		t.Fatalf("parseContainerState: %v", err)
	}
	if !state.Exists || !state.Running || state.Name != "codefly-gitops-remote-local-abcd1234" {
		t.Fatalf("unexpected state: %+v", state)
	}
	if len(state.PortBindings) != 1 || state.PortBindings[0].HostIP != "127.0.0.1" {
		t.Fatalf("port bindings = %+v", state.PortBindings)
	}
	if !slices.Contains(state.Networks, "k3d-codefly-local") {
		t.Fatalf("networks = %v", state.Networks)
	}
}

func ownedState(t *testing.T, spec RemoteSpec, revision string, notAfter time.Time) ContainerState {
	t.Helper()
	labels := spec.Labels(revision, "sha256:deadbeef", notAfter)
	return ContainerState{
		Name:         spec.ContainerName,
		Running:      true,
		Image:        spec.Image,
		Labels:       labels,
		Networks:     []string{spec.Network},
		PortBindings: []PortBinding{{ContainerPort: "443/tcp", HostIP: spec.HostAddr, HostPort: "50000"}},
		Exists:       true,
	}
}

func TestAuditStateFlagsDrift(t *testing.T) {
	spec := mustSpec(t, testConfig())
	now := time.Now()

	// A container without our role is ignored entirely.
	if findings := AuditState(&ContainerState{Exists: true, Labels: map[string]string{}}, now); len(findings) != 0 {
		t.Fatalf("audited a non-owned container: %v", findings)
	}

	state := ownedState(t, spec, "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", now.Add(time.Hour))
	state.PortBindings[0].HostIP = "0.0.0.0"
	state.Image = "nginx:latest"
	state.Running = false
	state.Labels[labelCertNotAfter] = now.Add(-time.Hour).Format(time.RFC3339)

	codes := map[string]bool{}
	for _, finding := range AuditState(&state, now) {
		codes[finding.Code] = true
	}
	for _, want := range []string{"wildcard-binding", "mutable-image", "expired-certificate", "not-running"} {
		if !codes[want] {
			t.Fatalf("missing finding %q in %v", want, codes)
		}
	}
}

func TestInspectFindingsHealthyAndDrift(t *testing.T) {
	spec := mustSpec(t, testConfig())
	now := time.Now()
	revision := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	healthy := ownedState(t, spec, revision, now.Add(time.Hour))
	if findings := InspectFindings(&spec, &healthy, revision, now); len(findings) != 0 {
		t.Fatalf("healthy remote produced findings: %+v", findings)
	}

	absent := InspectFindings(&spec, &ContainerState{Name: spec.ContainerName}, revision, now)
	if len(absent) != 1 || absent[0].Code != "absent" {
		t.Fatalf("absent remote findings = %+v", absent)
	}

	drifted := ownedState(t, spec, revision, now.Add(time.Hour))
	drifted.Networks = []string{"bridge"}
	drifted.Labels[labelOwner] = "mallory"
	delete(drifted.Labels, labelCAFingerprint)
	codes := map[string]bool{}
	for _, finding := range InspectFindings(&spec, &drifted, "deadbeef", now) {
		codes[finding.Code] = true
	}
	for _, want := range []string{"ownership-drift", "network-drift", "missing-ca-trust", "stale-remote"} {
		if !codes[want] {
			t.Fatalf("missing %q in %v", want, codes)
		}
	}
}

func TestValidateOwnershipRefusesDrift(t *testing.T) {
	spec := mustSpec(t, testConfig())
	now := time.Now()
	revision := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	healthy := ownedState(t, spec, revision, now.Add(time.Hour))
	if err := validateOwnership(&spec, &healthy); err != nil {
		t.Fatalf("owned container refused: %v", err)
	}

	foreign := ownedState(t, spec, revision, now.Add(time.Hour))
	foreign.Labels[labelWorkspace] = "someone-else"
	if err := validateOwnership(&spec, &foreign); err == nil {
		t.Fatalf("expected refusal on ownership drift")
	}

	// Owner is out of the container name but still a validated identity: a
	// container labeled with a different owner must not be torn down.
	otherOwner := ownedState(t, spec, revision, now.Add(time.Hour))
	otherOwner.Labels[labelOwner] = "intruder"
	if err := validateOwnership(&spec, &otherOwner); err == nil {
		t.Fatalf("expected refusal on owner drift")
	}

	detached := ownedState(t, spec, revision, now.Add(time.Hour))
	detached.Networks = []string{"host"}
	if err := validateOwnership(&spec, &detached); err == nil {
		t.Fatalf("expected refusal on network drift")
	}
}

func TestRemotePlanDescribesRemote(t *testing.T) {
	spec := mustSpec(t, testConfig())
	plan := spec.Plan("a1b2c3d4e5f60718293a4b5c6d7e8f9012345678")
	if plan.ContainerName != spec.ContainerName || plan.Image != spec.Image {
		t.Fatalf("plan identity mismatch: %+v", plan)
	}
	if !strings.HasPrefix(plan.HostBinding, spec.HostAddr+":") {
		t.Fatalf("plan host binding not loopback: %q", plan.HostBinding)
	}
	if plan.ArgoRepository != spec.ArgoRepository() {
		t.Fatalf("plan argo repository = %q", plan.ArgoRepository)
	}
	if plan.Labels[labelRole] != remoteRole {
		t.Fatalf("plan labels missing role: %v", plan.Labels)
	}
}

func fetchRemoteWorkspace(t *testing.T, remote, clusterContext string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	config := fmt.Sprintf(`name: payments
layout: flat
services:
  - name: api
environments:
  - name: local
    cluster:
      kind: k3d
      context: %q
gitops:
  repo-url: file://%s
  path: environments
  branch: main
`, clusterContext, remote)
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestNewFetchRemoteRequiresClusterContext(t *testing.T) {
	remote := createBareRepository(t)
	workspace := fetchRemoteWorkspace(t, remote, "")
	if _, err := NewFetchRemote(workspace, "local"); err == nil {
		t.Fatalf("expected error when cluster.context is unset (network cannot be derived)")
	}
}

func TestNewFetchRemoteRejectsNonK3dContext(t *testing.T) {
	remote := createBareRepository(t)
	workspace := fetchRemoteWorkspace(t, remote, "docker-desktop")
	if _, err := NewFetchRemote(workspace, "local"); err == nil {
		t.Fatalf("expected error for a non-k3d cluster.context")
	}
}

func TestNewFetchRemoteDerivesNetworkFromContext(t *testing.T) {
	remote := createBareRepository(t)
	workspace := fetchRemoteWorkspace(t, remote, "k3d-mycluster")
	fetch, err := NewFetchRemote(workspace, "local")
	if err != nil {
		t.Fatalf("NewFetchRemote: %v", err)
	}
	if fetch.Spec.Cluster != "mycluster" {
		t.Fatalf("cluster = %q, want mycluster", fetch.Spec.Cluster)
	}
	if fetch.Spec.Network != "k3d-mycluster" {
		t.Fatalf("network = %q, want k3d-mycluster", fetch.Spec.Network)
	}
}

// TestMirrorServesReviewedRevision exercises the git-only mirror path with a
// real repository: the mirror disables auto-gc, pins the reviewed revision to
// the serving ref, advertises it for dumb HTTP, and keeps serving a revision
// that is no longer the source tip.
func TestMirrorServesReviewedRevision(t *testing.T) {
	source := createBareRepository(t)
	first := runExternal(t, "", nil, "git", "--git-dir", source, "rev-parse", "refs/heads/main")

	spec, err := NewRemoteSpec(&RemoteConfig{
		WorkspaceRoot:  t.TempDir(),
		Owner:          "ada",
		Workspace:      "payments",
		Environment:    "local",
		Cluster:        "codefly-local",
		RepositorySlug: "codefly-test/manifests",
		SourceRepo:     "file://" + source,
		Image:          testImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	fetch := &FetchRemote{Spec: spec}

	if err := fetch.mirror(context.Background(), first); err != nil {
		t.Fatalf("mirror (fresh): %v", err)
	}
	if gc := runExternal(t, "", nil, "git", "--git-dir", spec.RepoDir(), "config", "--get", "gc.auto"); gc != "0" {
		t.Fatalf("mirror gc.auto = %q, want 0 (serving objects must not be auto-pruned)", gc)
	}
	if served := runExternal(t, "", nil, "git", "--git-dir", spec.RepoDir(), "rev-parse", remoteServeRef); served != first {
		t.Fatalf("serving ref = %q, want %q", served, first)
	}
	refs, err := os.ReadFile(filepath.Join(spec.RepoDir(), "info", "refs"))
	if err != nil {
		t.Fatalf("read info/refs: %v", err)
	}
	if !refsAdvertisesRevision(refs, first) {
		t.Fatalf("dumb-HTTP info/refs does not advertise the reviewed revision")
	}

	// Advance the source, then serve the earlier (non-tip) revision. It must
	// survive the fetch+prune and remain servable.
	work := t.TempDir()
	gitRun(t, "", "clone", source, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(work, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "second.txt")
	gitRun(t, work, "commit", "-m", "second")
	gitRun(t, work, "push", "origin", "main")

	if err := fetch.mirror(context.Background(), first); err != nil {
		t.Fatalf("mirror (serve non-tip revision after update): %v", err)
	}
	if served := runExternal(t, "", nil, "git", "--git-dir", spec.RepoDir(), "rev-parse", remoteServeRef); served != first {
		t.Fatalf("serving ref after update = %q, want the pinned %q", served, first)
	}
}
