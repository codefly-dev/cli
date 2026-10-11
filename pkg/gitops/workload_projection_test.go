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
			if token["audience"] != "accounts" || token["path"] != "token" {
				t.Fatalf("the projected token is %v, want audience accounts at token", token)
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

// TestAWorkloadThatAlreadyProjectsTheTokenIsLeftAsItIs: an agent that
// projects the host-audience token itself keeps its own projection; the
// render adds no second one.
func TestAWorkloadThatAlreadyProjectsTheTokenIsLeftAsItIs(t *testing.T) {
	own := strings.Replace(pinnedDeployment, "      containers:\n", "      volumes:\n        - name: own-token\n          projected:\n            sources:\n              - serviceAccountToken:\n                  audience: accounts\n                  path: token\n      containers:\n", 1)
	own += "          volumeMounts:\n            - name: own-token\n              mountPath: /var/run/secrets/own\n"
	destination := filepath.Join(t.TempDir(), "modules", "crm")
	if _, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), kustomizedWorkload(own)); err != nil {
		t.Fatal(err)
	}
	if volume, _ := renderedVolume(t, destination, identityTokenVolume); volume != nil {
		t.Fatal("a second identity token was projected beside the agent's own")
	}
}

// TestALocalHostMountsItsTrustAnchorWhereItReadsIt: on a k3d environment that
// states the host's trust-anchor path, the host's verifier — the service
// host.delivery names — mounts the local trust anchor there: one projected
// volume whose only sources are the local trust ConfigMap and Secret, keys not
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
	if len(sources) != 2 ||
		mapField(sources[0].(map[string]any), "configMap")["name"] != LocalTrustAnchorObjects ||
		mapField(sources[1].(map[string]any), "secret")["name"] != LocalTrustAnchorObjects {
		t.Fatalf("the trust anchor's sources are %v, want the local trust ConfigMap and Secret alone", sources)
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
