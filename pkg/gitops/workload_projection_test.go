package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
)

const sidecarDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:
      serviceAccountName: api
      containers:
        - name: api
          image: ghcr.io/codefly-dev/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        - name: proxy
          image: ghcr.io/codefly-dev/proxy@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`

// kustomizedWorkload writes a unit the way an agent renders it: the workload
// in a base, and an environment overlay that is a kustomization over it.
func kustomizedWorkload(body string) func(context.Context, string) error {
	return func(_ context.Context, root string) error {
		base := filepath.Join(root, "services", "api", "base")
		overlay := filepath.Join(root, "services", "api", "overlays", "prod")
		for _, dir := range []string{base, overlay} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(base, "deployment.yaml"), []byte(body), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(base, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"), 0o644)
	}
}

// renderedVolume reads the rendered unit back as Kubernetes would run it
// and returns, per container, the mount paths of the named volume, with the
// volume itself.
func renderedVolume(t *testing.T, destination, volume string) (map[string]any, map[string][]map[string]any) {
	t.Helper()
	manifests, err := overlayManifests(filepath.Join(destination, "services", "api", "overlays", "prod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range manifests {
		if item.kind != kindDeployment {
			continue
		}
		spec, _ := podSpec(item)
		var declared map[string]any
		for _, entry := range sliceField(spec, "volumes") {
			if candidate, _ := entry.(map[string]any); candidate["name"] == volume {
				declared = candidate
			}
		}
		mounts := map[string][]map[string]any{}
		for _, entry := range sliceField(spec, "containers") {
			container, _ := entry.(map[string]any)
			for _, raw := range sliceField(container, "volumeMounts") {
				if mount, _ := raw.(map[string]any); mount["name"] == volume {
					mounts[container["name"].(string)] = append(mounts[container["name"].(string)], mount)
				}
			}
		}
		return declared, mounts
	}
	t.Fatal("the rendered unit carries no Deployment")
	return nil, nil
}

// TestEveryWorkloadAHostAdmitsProjectsItsIdentityToken: the pod a presence
// document declares proves who it is with a ServiceAccount token projected
// for the host's audience (the mint saas#1056 binds to the running build) —
// mounted at the path the SDK reads, into the authenticating container alone,
// in an agent's kustomized overlay and in a plain one alike, and pinned by the
// presence document like every rendered byte.
func TestEveryWorkloadAHostAdmitsProjectsItsIdentityToken(t *testing.T) {
	for name, generate := range map[string]func(context.Context, string) error{
		"a kustomized overlay":     kustomizedWorkload(sidecarDeployment),
		"an overlay of manifests":  renderWorkload(sidecarDeployment),
		"a single-container agent": kustomizedWorkload(pinnedDeployment),
	} {
		t.Run(name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "modules", "crm")
			if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), generate); err != nil {
				t.Fatal(err)
			}
			volume, mounts := renderedVolume(t, destination, identityTokenVolume)
			if volume == nil {
				t.Fatal("the workload projects no identity token")
			}
			token := mapField(sliceField(mapField(volume, "projected"), "sources")[0].(map[string]any), "serviceAccountToken")
			if token["audience"] != "accounts" || token["path"] != "token" || integerField(token, "expirationSeconds") != 600 {
				t.Fatalf("the projected token is %v, want audience accounts at token, living 600 s", token)
			}
			if len(mounts) != 1 || len(mounts["api"]) != 1 || mounts["api"][0]["mountPath"] != "/var/run/secrets/codefly" || mounts["api"][0]["readOnly"] != true {
				t.Fatalf("the token is mounted as %v, want into the authenticating container alone, read-only, at the SDK's path", mounts)
			}
			binding := deliveredBinding(t, destination, "example.prod.crm")
			if len(binding.Workloads) != 1 || binding.Workloads[0].Identity.Audience != "accounts" {
				t.Fatalf("the presence document does not declare the identity the pod presents: %+v", binding.Workloads)
			}
		})
	}
}

// withTokenMount is pinnedDeployment whose api container mounts a projected
// token for audience at mountPath, the token at file inside the volume, with
// subPath when set.
func withTokenMount(audience, mountPath, file, subPath string) string {
	return withTokenLifetime(audience, mountPath, file, subPath, "600")
}

// withTokenLifetime is withTokenMount with the projection's expirationSeconds
// ("" leaves it unset, which the kubelet reads as 3600).
func withTokenLifetime(audience, mountPath, file, subPath, lifetime string) string {
	expiration := ""
	if lifetime != "" {
		expiration = "                  expirationSeconds: " + lifetime + "\n"
	}
	body := strings.Replace(pinnedDeployment, "      containers:\n", "      volumes:\n        - name: own-token\n          projected:\n            sources:\n              - serviceAccountToken:\n                  audience: "+audience+"\n"+expiration+"                  path: "+file+"\n      containers:\n", 1)
	body += "          volumeMounts:\n            - name: own-token\n              mountPath: " + mountPath + "\n"
	if subPath != "" {
		body += "              subPath: " + subPath + "\n"
	}
	return body
}

// TestAnExistingTokenIsReusedOnlyWhereTheSDKReadsIt: the render reuses a
// workload's own projection only when it provides the rotating host-audience
// token at exactly the SDK's path. Anywhere else, ours is added beside it; a
// mount that occupies the path with something else — another audience, a
// subPath (never refreshed, so the token stops rotating), another volume — is
// refused with what is there, since adding ours would shadow it or be
// shadowed.
func TestAnExistingTokenIsReusedOnlyWhereTheSDKReadsIt(t *testing.T) {
	render := func(t *testing.T, body string) (string, error) {
		t.Helper()
		destination := filepath.Join(t.TempDir(), "modules", "crm")
		_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), kustomizedWorkload(body))
		return destination, err
	}
	destination, err := render(t, withTokenMount("accounts", "/var/run/secrets/codefly", "token", ""))
	if err != nil {
		t.Fatal(err)
	}
	if volume, _ := renderedVolume(t, destination, identityTokenVolume); volume != nil {
		t.Fatal("a second token was projected beside one already at the SDK's path")
	}

	destination, err = render(t, withTokenMount("accounts", "/var/run/secrets/own", "token", ""))
	if err != nil {
		t.Fatal(err)
	}
	if volume, mounts := renderedVolume(t, destination, identityTokenVolume); volume == nil || mounts["api"][0]["mountPath"] != identityTokenMount {
		t.Fatalf("a token elsewhere suppressed the one at the SDK's path: %v %v", volume, mounts)
	}

	// A host refuses a mint token living longer than 600 s: a projection at
	// the right path that lives longer is refused, never kept.
	for name, lifetime := range map[string]string{"an hour": "3600", "unset (the kubelet's hour)": ""} {
		if _, err = render(t, withTokenLifetime("accounts", "/var/run/secrets/codefly", "token", "", lifetime)); err == nil || !strings.Contains(err.Error(), "expirationSeconds: 600") {
			t.Errorf("%s: a token outliving the host's limit was kept: %v", name, err)
		}
	}

	for name, body := range map[string]string{
		"a subPath mount of the token": withTokenMount("accounts", "/var/run/secrets/codefly/token", "token", "token"),
		"another audience at the path": withTokenMount("other", "/var/run/secrets/codefly", "token", ""),
		"another volume at the path":   withTokenMount("accounts", "/var/run/secrets/codefly", "jwt", ""),
		"a volume above the path":      withTokenMount("accounts", "/var/run/secrets", "token", ""),
	} {
		if _, err = render(t, body); err == nil || !strings.Contains(err.Error(), "/var/run/secrets/codefly/token") {
			t.Errorf("%s: not refused with the path it occupies: %v", name, err)
		}
	}
}

// TestALocalHostMountsItsTrustAnchorWhereItReadsIt: on a k3d environment that
// states the host's trust-anchor path, the host's verifier — the service
// host.delivery names — mounts the local trust anchor there: one projected
// volume whose only sources are the local trust ConfigMap, keys not
// remapped, read-only, in the authenticating container alone. Any other
// environment mounts nothing: a deployed host's anchor is its platform's.
func TestALocalHostMountsItsTrustAnchorWhereItReadsIt(t *testing.T) {
	const anchor = "/etc/host/delivery-trust"
	options := func(destination, kind string) *RenderOptions {
		opts := solutionRenderOptions(destination)
		opts.Host.Delivery = "crm/api/rest"
		opts.Host.TrustAnchor = anchor
		opts.Target = &environments.Environment{Name: "prod", Cluster: &environments.EnvironmentCluster{Kind: kind, Context: "k3d-dev"}}
		return opts
	}
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), options(destination, "k3d"), kustomizedWorkload(sidecarDeployment)); err != nil {
		t.Fatal(err)
	}
	volume, mounts := renderedVolume(t, destination, LocalTrustAnchorVolume)
	if volume == nil {
		t.Fatal("the local host's verifier mounts no trust anchor")
	}
	sources := sliceField(mapField(volume, "projected"), "sources")
	if len(sources) != 1 ||
		mapField(sources[0].(map[string]any), "configMap")["name"] != LocalTrustAnchorObjects {
		t.Fatalf("the trust anchor's sources are %v, want the local trust ConfigMap alone", sources)
	}
	for _, source := range sources {
		for _, object := range source.(map[string]any) {
			if _, remapped := object.(map[string]any)["items"]; remapped {
				t.Fatal("the trust anchor remaps its keys")
			}
		}
	}
	if len(mounts) != 1 || len(mounts["api"]) != 1 || mounts["api"][0]["mountPath"] != anchor || mounts["api"][0]["readOnly"] != true {
		t.Fatalf("the trust anchor is mounted as %v, want into the verifier's container alone, read-only, at %s", mounts, anchor)
	}

	hosted := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), options(hosted, "gke"), kustomizedWorkload(sidecarDeployment)); err != nil {
		t.Fatal(err)
	}
	if volume, _ := renderedVolume(t, hosted, LocalTrustAnchorVolume); volume != nil {
		t.Fatal("a hosted environment's render mounted a local trust anchor")
	}
}

// TestPresenceDeclaresEveryContainerOfTheWorkload: the presence document
// names every container of a workload a host admits — the authenticating one
// with its image digest, every sidecar and init container as one never
// accepted as it — and every one of them must be pinned by digest, or the
// render refuses. (Core's presence schema, codefly/solution-host-binding/v1
// through core v0.18.1, carries the other containers by NAME only; their
// digests are read here and cannot be declared until the schema carries them.)
func TestPresenceDeclaresEveryContainerOfTheWorkload(t *testing.T) {
	withInit := strings.Replace(sidecarDeployment, "      containers:\n", "      initContainers:\n        - name: migrate\n          image: ghcr.io/codefly-dev/migrate@sha256:"+strings.Repeat("c", 64)+"\n      containers:\n", 1)
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), kustomizedWorkload(withInit)); err != nil {
		t.Fatal(err)
	}
	workload := deliveredBinding(t, destination, "example.prod.crm").Workloads[0]
	if workload.Container != "api" || workload.NonAuthenticating == nil || strings.Join(*workload.NonAuthenticating, ",") != "migrate,proxy" {
		t.Fatalf("the presence document declares %s and %v, want api and every other container (migrate, proxy)", workload.Container, workload.NonAuthenticating)
	}
	unpinned := strings.Replace(withInit, "ghcr.io/codefly-dev/proxy@sha256:"+strings.Repeat("b", 64), "ghcr.io/codefly-dev/proxy:latest", 1)
	if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm")), kustomizedWorkload(unpinned)); err == nil || !strings.Contains(err.Error(), "not pinned by digest") {
		t.Fatalf("a sidecar running an unpinned image was rendered: %v", err)
	}
}

// TestAProjectionAnotherMountHidesIsNeverKept is the review's attack: the
// workload's own token is exactly where the SDK reads it, but another mount
// hides it — an emptyDir at the token's path, or at the projection's own
// directory. The token the SDK would read is not the token; the render
// refuses with the mount that hides it rather than keeping the projection.
func TestAProjectionAnotherMountHidesIsNeverKept(t *testing.T) {
	for name, shadow := range map[string]string{
		"at the token's path":         "/var/run/secrets/codefly/token",
		"at the projection's own dir": "/var/run/secrets/codefly",
	} {
		body := withTokenMount("accounts", "/var/run/secrets/codefly", "token", "")
		body = strings.Replace(body, "      volumes:\n", "      volumes:\n        - name: scratch\n          emptyDir: {}\n", 1)
		body += "            - name: scratch\n              mountPath: " + shadow + "\n"
		_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm")), kustomizedWorkload(body))
		if err == nil || !strings.Contains(err.Error(), "volume scratch at "+shadow) {
			t.Errorf("%s: a hidden token was kept: %v", name, err)
		}
	}
}

// TestTheTrustAnchorAnnotationIsReservedForTheLocalRun: a local run stamps
// codefly.dev/trust-anchor on the pod templates that read its trust anchor,
// and a value Argo CD owned in desired state would revert that stamp — and
// with it a revocation. The render refuses a workload carrying it.
func TestTheTrustAnchorAnnotationIsReservedForTheLocalRun(t *testing.T) {
	owned := strings.Replace(pinnedDeployment, "      labels:\n        app: api\n", "      labels:\n        app: api\n      annotations:\n        codefly.dev/trust-anchor: sha256:pinned-in-git\n", 1)
	if owned == pinnedDeployment {
		t.Fatal("the fixture's pod template was not found")
	}
	_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm")), kustomizedWorkload(owned))
	if err == nil || !strings.Contains(err.Error(), TrustAnchorAnnotation) {
		t.Fatalf("a workload owning the run's annotation in desired state was rendered: %v", err)
	}
}
