package tenants

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const baseKustomization = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - virtualservice.yaml
  - external-secret.yaml
`

const baseVirtualService = `apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: api
spec:
  hosts:
    - base.example.com
  gateways:
    - mesh
`

const baseExternalSecret = `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: api-store
spec:
  secretStoreRef:
    kind: ClusterSecretStore
    name: base
  target:
    name: api-store
  data:
    - secretKey: connection
      remoteRef:
        key: api/store
        property: connection
`

func writeBase(t *testing.T, root string) {
	t.Helper()
	base := filepath.Join(root, "base")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"kustomization.yaml":   baseKustomization,
		"virtualservice.yaml":  baseVirtualService,
		"external-secret.yaml": baseExternalSecret,
	} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeModel(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "tenants.codefly.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildOverlay runs the generated overlay through Kustomize and returns the
// VirtualService hosts and the ExternalSecret store name so the test asserts on
// the real patched output rather than the generated YAML text.
func buildOverlay(t *testing.T, root, overlay string) ([]string, string) {
	t.Helper()
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	resMap, err := kustomizer.Run(filesys.MakeFsOnDisk(), filepath.Join(root, "overlays", overlay))
	if err != nil {
		t.Fatalf("kustomize build %s: %v", overlay, err)
	}
	out, err := resMap.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	var store string
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		switch doc["kind"] {
		case "VirtualService":
			spec, _ := doc["spec"].(map[string]any)
			for _, h := range spec["hosts"].([]any) {
				hosts = append(hosts, h.(string))
			}
		case "ExternalSecret":
			spec, _ := doc["spec"].(map[string]any)
			ref, _ := spec["secretStoreRef"].(map[string]any)
			store, _ = ref["name"].(string)
		}
	}
	return hosts, store
}

func TestGenerateProducesPatchedOverlays(t *testing.T) {
	root := t.TempDir()
	writeBase(t, root)
	modelPath := writeModel(t, root, `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
    secret-store: acme-aws
  - name: acme
    cloud: gcp
    host: acme.gcp.example.com
    secret-store: acme-gcp
  - name: globex
    cloud: aws
    host: globex.example.com
    secret-store: globex-aws
`)

	model, err := LoadModel(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	written, err := Generate(root, model)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme-aws", "acme-gcp", "globex-aws"}
	if len(written) != len(want) {
		t.Fatalf("wrote %v, want %v", written, want)
	}
	for i := range want {
		if written[i] != want[i] {
			t.Fatalf("wrote %v, want %v", written, want)
		}
	}

	cases := map[string]struct {
		host  string
		store string
	}{
		"acme-aws":   {"acme.example.com", "acme-aws"},
		"acme-gcp":   {"acme.gcp.example.com", "acme-gcp"},
		"globex-aws": {"globex.example.com", "globex-aws"},
	}
	for overlay, want := range cases {
		hosts, store := buildOverlay(t, root, overlay)
		if len(hosts) != 1 || hosts[0] != want.host {
			t.Errorf("%s hosts = %v, want [%s]", overlay, hosts, want.host)
		}
		if store != want.store {
			t.Errorf("%s secret store = %q, want %q", overlay, store, want.store)
		}
	}
}

func TestGenerateWithoutSecretStoreLeavesBaseRef(t *testing.T) {
	root := t.TempDir()
	writeBase(t, root)
	modelPath := writeModel(t, root, `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
`)
	model, err := LoadModel(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, model); err != nil {
		t.Fatal(err)
	}
	hosts, store := buildOverlay(t, root, "acme-aws")
	if len(hosts) != 1 || hosts[0] != "acme.example.com" {
		t.Errorf("hosts = %v, want [acme.example.com]", hosts)
	}
	if store != "base" {
		t.Errorf("secret store = %q, want base (unpatched)", store)
	}
}

func TestGenerateOverwritesExistingOverlay(t *testing.T) {
	root := t.TempDir()
	writeBase(t, root)
	stale := filepath.Join(root, "overlays", "acme-aws", "kustomization.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	modelPath := writeModel(t, root, `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
`)
	model, err := LoadModel(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, model); err != nil {
		t.Fatal(err)
	}
	hosts, _ := buildOverlay(t, root, "acme-aws")
	if len(hosts) != 1 || hosts[0] != "acme.example.com" {
		t.Errorf("stale overlay not overwritten: hosts = %v", hosts)
	}
}

func TestLoadModelValidation(t *testing.T) {
	cases := map[string]string{
		"wrong schema": `schema-version: codefly.dev/tenant-model/v2
base: base
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
`,
		"no base": `schema-version: codefly.dev/tenant-model/v1
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
`,
		"escaping base": `schema-version: codefly.dev/tenant-model/v1
base: ../escape
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
`,
		"no tenants": `schema-version: codefly.dev/tenant-model/v1
base: base
tenants: []
`,
		"missing host": `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
`,
		"wildcard host": `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
    host: "*.example.com"
`,
		"invalid name": `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: Acme_Corp
    cloud: aws
    host: acme.example.com
`,
		"duplicate overlay": `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
  - name: acme
    cloud: aws
    host: other.example.com
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := writeModel(t, root, body)
			if _, err := LoadModel(path); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestGenerateMissingBase(t *testing.T) {
	root := t.TempDir()
	modelPath := writeModel(t, root, `schema-version: codefly.dev/tenant-model/v1
base: base
tenants:
  - name: acme
    cloud: aws
    host: acme.example.com
`)
	model, err := LoadModel(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, model); err == nil {
		t.Fatal("expected error when base directory is absent")
	}
}
