package gitops

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// serviceUnitFiles is the shape a service agent stages for one unit: a base
// kustomization listing its resources, and an environment overlay on top. The
// namespace.yaml is what a service agent emits when its deployment template
// never adopted the restricted-profile guard every other agent carries.
func serviceUnitFiles(namespace, environment string) map[string]string {
	return map[string]string{
		filepath.Join("base", "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - namespace.yaml\n  - deployment.yaml\n",
		filepath.Join("base", "namespace.yaml"): "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: \"" + namespace + "\"\n" +
			"  labels:\n    app.kubernetes.io/managed-by: codefly\n",
		filepath.Join("base", "deployment.yaml"): pinnedDeployment,
		filepath.Join("overlays", environment, "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - ../../base\n",
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for relative, content := range files {
		full := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func manifestKinds(t *testing.T, root string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		if relative == InventoryFilename || !isManifestFile(relative) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests, _, err := decodeYAML(filepath.ToSlash(relative), data)
		if err != nil {
			return err
		}
		for _, item := range manifests {
			found[item.kind] = append(found[item.kind], metadataString(item.value, "name"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for kind := range found {
		sort.Strings(found[kind])
	}
	return found
}

// A governed cell provisions its namespaces out of band: its AppProject has an
// empty clusterResourceWhitelist, so an Application shipping a Namespace is
// refused with "resource :Namespace is not permitted in project <name>", while a
// render given no AppProject at all refuses on its own cluster-scope rule. The
// render must take neither horn: a service unit's Namespace for the namespace
// the render deploys into is dropped, the render still succeeds, and the tree it
// installs is valid without it.
func TestPromotableRenderDropsAServiceUnitNamespaceForItsOwnDestination(t *testing.T) {
	const namespace = "platform-obin-wiki"
	destination := filepath.Join(t.TempDir(), "wiki")

	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: destination, Module: "wiki", UnitNames: []string{"backend"},
		Environment: "staging", Namespace: namespace, AppProject: "platform-obin", Promotable: true,
	}, func(_ context.Context, root string) error {
		writeFiles(t, filepath.Join(root, serviceUnitDir, "backend"), serviceUnitFiles(namespace, "staging"))
		return nil
	})
	if err != nil {
		t.Fatalf("render refused an externally provisioned namespace: %v", err)
	}

	if kinds := manifestKinds(t, result.Path); len(kinds["Namespace"]) != 0 {
		t.Fatalf("render emitted Namespace manifests %v for an externally provisioned namespace", kinds["Namespace"])
	}
	if _, err := os.Stat(filepath.Join(result.Path, serviceUnitDir, "backend", "base", "namespace.yaml")); !os.IsNotExist(err) {
		t.Fatalf("namespace.yaml survived the render: %v", err)
	}
	base, err := os.ReadFile(filepath.Join(result.Path, serviceUnitDir, "backend", "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "namespace.yaml") {
		t.Fatalf("base kustomization still references the dropped manifest:\n%s", base)
	}
	if strings.Count(string(base), "deployment.yaml") != 1 {
		t.Fatalf("base kustomization lost a resource it still owns:\n%s", base)
	}
	if len(result.ElidedNamespaces) != 1 || !strings.HasSuffix(result.ElidedNamespaces[0], "base/namespace.yaml") {
		t.Fatalf("render did not report what it dropped: %v", result.ElidedNamespaces)
	}
	// The installed tree is the committed output: it has to validate, and to
	// kustomize-build, without the manifest that was dropped.
	if err := ValidateRenderedTree(result.Path, "platform-obin", true); err != nil {
		t.Fatalf("validate installed tree: %v", err)
	}
}

// The other horn of the bind: with no AppProject named, the same tree refused
// outright ("cluster-scoped Namespace is outside an AppProject contract"). With
// nothing claiming the namespace there is nothing left to refuse, so a workspace
// whose namespaces are pre-created externally renders either way.
func TestPromotableRenderWithoutAnAppProjectNoLongerRefusesTheProvisionedNamespace(t *testing.T) {
	const namespace = "platform-obin-wiki"
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "wiki"), Module: "wiki", UnitNames: []string{"backend"},
		Environment: "staging", Namespace: namespace, Promotable: true,
	}, func(_ context.Context, root string) error {
		writeFiles(t, filepath.Join(root, serviceUnitDir, "backend"), serviceUnitFiles(namespace, "staging"))
		return nil
	})
	if err != nil {
		t.Fatalf("render refused without an AppProject: %v", err)
	}
	if kinds := manifestKinds(t, result.Path); len(kinds["Namespace"]) != 0 {
		t.Fatalf("render emitted Namespace manifests %v", kinds["Namespace"])
	}
}

// The elision is exact. A Namespace naming anything other than the render's own
// destination is a genuine cluster-level claim: it stays in the tree and keeps
// facing the AppProject contract check, which is the boundary being protected.
func TestPromotableRenderKeepsAServiceUnitNamespaceNamingAnotherNamespace(t *testing.T) {
	const namespace = "platform-obin-wiki"
	files := serviceUnitFiles(namespace, "staging")
	files[filepath.Join("base", "kustomization.yaml")] = "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\nresources:\n  - namespace.yaml\n  - elsewhere.yaml\n  - deployment.yaml\n"
	files[filepath.Join("base", "elsewhere.yaml")] = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: platform-obin-model\n"

	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "wiki"), Module: "wiki", UnitNames: []string{"backend"},
		Environment: "staging", Namespace: namespace, AppProject: "platform-obin", Promotable: true,
	}, func(_ context.Context, root string) error {
		writeFiles(t, filepath.Join(root, serviceUnitDir, "backend"), files)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := manifestKinds(t, result.Path)
	if len(kinds["Namespace"]) != 1 || kinds["Namespace"][0] != "platform-obin-model" {
		t.Fatalf("Namespace manifests after render = %v, want only platform-obin-model", kinds["Namespace"])
	}
}

// A solution owns the namespace it renders into — nothing else shares it, and
// the AppProject its publish generates authorizes it — so the service-unit rule
// must not reach across into a solution unit.
func TestPromotableRenderKeepsASolutionUnitNamespace(t *testing.T) {
	const namespace = "lastlogin-go"
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "lastlogin-go"), Module: "lastlogin-go",
		Units: []InventoryUnit{{
			Kind: UnitKindSolution, Module: "lastlogin-go", Name: "lastlogin-go",
			Path: filepath.ToSlash(filepath.Join(solutionUnitDir, "lastlogin-go")),
		}},
		Environment: "staging", Namespace: namespace, AppProject: "lastlogin-go", Promotable: true,
	}, func(_ context.Context, root string) error {
		writeFiles(t, filepath.Join(root, solutionUnitDir, "lastlogin-go"), serviceUnitFiles(namespace, "staging"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := manifestKinds(t, result.Path)
	if len(kinds["Namespace"]) != 1 || kinds["Namespace"][0] != namespace {
		t.Fatalf("solution lost the namespace it owns: %v", kinds["Namespace"])
	}
	if len(result.ElidedNamespaces) != 0 {
		t.Fatalf("solution unit was elided: %v", result.ElidedNamespaces)
	}
}

// A single-service render stages its units one directory deeper
// ("modules/<module>/services/<service>"). The rule follows the unit directory,
// not a fixed depth, so that layout is covered by the same pass.
func TestPromotableServiceRenderDropsTheNamespaceInItsNestedUnitLayout(t *testing.T) {
	const namespace = "platform-obin-wiki"
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(t.TempDir(), "backend"), Module: "wiki", Unit: "backend",
		Environment: "staging", Namespace: namespace, AppProject: "platform-obin", Promotable: true,
	}, func(_ context.Context, root string) error {
		writeFiles(t, filepath.Join(root, "modules", "wiki", serviceUnitDir, "backend"), serviceUnitFiles(namespace, "staging"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if kinds := manifestKinds(t, result.Path); len(kinds["Namespace"]) != 0 {
		t.Fatalf("nested service unit kept its Namespace: %v", kinds["Namespace"])
	}
}

// A file carrying the Namespace alongside other documents keeps those documents:
// the elision is per-document, not per-file.
func TestElideProvisionedNamespaceKeepsTheOtherDocumentsOfAFile(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, serviceUnitDir, "backend")
	writeFiles(t, unit, map[string]string{
		filepath.Join("base", "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - bundle.yaml\n",
		filepath.Join("base", "bundle.yaml"): "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: shared\n---\n" + pinnedDeployment,
	})

	elided, err := elideProvisionedNamespaces(root, &RenderOptions{Promotable: true, Namespace: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if len(elided) != 1 {
		t.Fatalf("elided = %v, want one entry", elided)
	}
	kinds := manifestKinds(t, unit)
	if len(kinds["Namespace"]) != 0 || len(kinds["Deployment"]) != 1 {
		t.Fatalf("document filter changed the wrong documents: %v", kinds)
	}
	base, err := os.ReadFile(filepath.Join(unit, "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(base), "bundle.yaml") {
		t.Fatalf("kustomization lost a file that still exists:\n%s", base)
	}
}

// A render that is not promotable, or that has no destination namespace of its
// own, is left exactly as the generator produced it.
func TestElideProvisionedNamespacesOnlyRunsForAPromotableNamespacedRender(t *testing.T) {
	for _, testCase := range []struct {
		name string
		opts RenderOptions
	}{
		{name: "not promotable", opts: RenderOptions{Namespace: "shared"}},
		{name: "no namespace", opts: RenderOptions{Promotable: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, filepath.Join(root, serviceUnitDir, "backend"), serviceUnitFiles("shared", "staging"))
			elided, err := elideProvisionedNamespaces(root, &testCase.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(elided) != 0 {
				t.Fatalf("elided = %v, want none", elided)
			}
			if kinds := manifestKinds(t, root); len(kinds["Namespace"]) != 1 {
				t.Fatalf("tree changed: %v", kinds)
			}
		})
	}
}
