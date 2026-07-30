package gitops

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	if first.HostPort != second.HostPort {
		t.Fatalf("host port not deterministic: %d vs %d", first.HostPort, second.HostPort)
	}
	if first.ContainerName != first.DNSName {
		t.Fatalf("dns name must equal container name, got %s and %s", first.ContainerName, first.DNSName)
	}
	if first.HostPort < 1 || first.HostPort > 65535 {
		t.Fatalf("host port out of range: %d", first.HostPort)
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
	wantPublish := "--publish 127.0.0.1:" // loopback only, never a wildcard
	if !strings.Contains(joined, wantPublish) {
		t.Fatalf("host binding is not loopback-only: %q", joined)
	}
	if strings.Contains(joined, "0.0.0.0") || strings.Contains(joined, "--publish :") {
		t.Fatalf("host binding leaks a wildcard: %q", joined)
	}
	if !strings.Contains(joined, "--network "+spec.Network) {
		t.Fatalf("missing private network: %q", joined)
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
