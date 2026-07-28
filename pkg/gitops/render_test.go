package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pinnedDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: api
          image: ghcr.io/codefly-dev/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

func TestRenderOwnedTreeIsDeterministicAndReplacesOnlyOwnedDestination(t *testing.T) {
	parent := t.TempDir()
	destination := filepath.Join(parent, "modules", "payments")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "stale.yaml"), []byte(pinnedDeployment), 0o644); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(parent, "README.md")
	if err := os.WriteFile(unowned, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	render := func(ctx context.Context, root string) error {
		if err := os.MkdirAll(filepath.Join(root, "services", "api"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "services", "api", "deployment.yaml"), []byte(pinnedDeployment), 0o644)
	}
	options := RenderOptions{
		Destination: destination, Module: "payments", Environment: "production", Promotable: true,
	}
	first, err := RenderOwnedTree(context.Background(), options, render)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderOwnedTree(context.Background(), options, render)
	if err != nil {
		t.Fatal(err)
	}
	if first.Inventory.Digest != second.Inventory.Digest {
		t.Fatalf("digest changed across identical renders: %s != %s", first.Inventory.Digest, second.Inventory.Digest)
	}
	if _, err := os.Stat(filepath.Join(destination, "stale.yaml")); !os.IsNotExist(err) {
		t.Fatalf("stale owned file remains: %v", err)
	}
	if data, err := os.ReadFile(unowned); err != nil || string(data) != "keep me" {
		t.Fatalf("unowned sibling changed: %q, %v", data, err)
	}
	if err := ValidateRenderedTree(destination, "", true); err != nil {
		t.Fatalf("validate installed tree: %v", err)
	}
}

func TestRenderValidationFailureLeavesPreviousTreeUntouched(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "owned")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(destination, "previous.yaml")
	if err := os.WriteFile(previous, []byte(pinnedDeployment), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := RenderOwnedTree(context.Background(), RenderOptions{
		Destination: destination, Module: "payments", Environment: "production", Promotable: true,
	}, func(ctx context.Context, root string) error {
		return os.WriteFile(filepath.Join(root, "secret.yaml"), []byte(`apiVersion: v1
kind: Secret
metadata:
  name: database
stringData:
  password: plaintext
`), 0o644)
	})
	if err == nil || !strings.Contains(err.Error(), "Secret values") {
		t.Fatalf("render error = %v, want Secret rejection", err)
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatalf("previous tree was replaced after validation failure: %v", err)
	}
}

func TestRenderInventoryMustRemainCanonical(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "owned")
	_, err := RenderOwnedTree(context.Background(), RenderOptions{
		Destination: destination, Module: "payments", Environment: "production", Promotable: true,
	}, func(ctx context.Context, root string) error {
		return os.WriteFile(filepath.Join(root, "deployment.yaml"), []byte(pinnedDeployment), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(destination, InventoryFilename)
	data, err := os.ReadFile(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inventory, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInventory(destination); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("non-canonical inventory error = %v", err)
	}
}

func TestRenderRejectsHostileRemoteOutput(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "unpinned image",
			manifest: strings.Replace(pinnedDeployment,
				"ghcr.io/codefly-dev/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"ghcr.io/codefly-dev/api:latest", 1),
			want: "not digest-pinned",
		},
		{
			name: "credential environment value",
			manifest: pinnedDeployment + `      initContainers:
        - name: migrate
          image: ghcr.io/codefly-dev/migrate@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
          env:
            - name: DATABASE_PASSWORD
              value: plaintext
`,
			want: "credential value",
		},
		{
			name: "unsafe URL",
			manifest: pinnedDeployment + `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: endpoint
data:
  url: http://api.example.com
`,
			want: "unsafe URL scheme",
		},
		{
			name: "URL credentials",
			manifest: pinnedDeployment + `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: endpoint
data:
  url: https://user:password@api.example.com
`,
			want: "URL contains credentials",
		},
		{
			name: "wildcard authority",
			manifest: pinnedDeployment + `---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: api
spec:
  rules:
    - host: "*.example.com"
`,
			want: "wildcard authority",
		},
		{
			name:     "placeholder",
			manifest: strings.Replace(pinnedDeployment, "name: api", "name: ${SERVICE_NAME}", 1),
			want:     "unresolved placeholder",
		},
		{
			name: "undeclared cluster scope",
			manifest: pinnedDeployment + `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: api
rules: []
`,
			want: "outside an AppProject contract",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RenderOwnedTree(context.Background(), RenderOptions{
				Destination: filepath.Join(t.TempDir(), "owned"),
				Module:      "payments", Environment: "production", Promotable: true,
			}, func(ctx context.Context, root string) error {
				return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(test.manifest), 0o644)
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRenderAllowsOnlyClusterScopeDeclaredBySelectedProject(t *testing.T) {
	manifests := pinnedDeployment + `---
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: payments
  namespace: argocd
spec:
  sourceRepos:
    - https://github.com/codefly-dev/manifests.git
  destinations:
    - namespace: payments
      server: https://kubernetes.default.svc
  clusterResourceWhitelist:
    - group: ""
      kind: Namespace
---
apiVersion: v1
kind: Namespace
metadata:
  name: payments
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: payments
  namespace: argocd
spec:
  project: payments
`
	_, err := RenderOwnedTree(context.Background(), RenderOptions{
		Destination: filepath.Join(t.TempDir(), "owned"), Module: "payments",
		Environment: "production", AppProject: "payments", Promotable: true,
	}, func(ctx context.Context, root string) error {
		return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(manifests), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
