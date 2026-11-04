package gitops

import (
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/routing"
	"gopkg.in/yaml.v3"
)

// STRICT mTLS IS A LAYER OF THE RENDER, NOT AN OPTION ON IT.
//
// The render delivers, with every module tree, the edges into that module's
// workloads: each endpoint and the mesh principals admitted to it
// (mesh_edges.go). A principal is the peer's ServiceAccount SPIFFE ID, and it
// is a fact about a connection only when the peer presented a mesh
// certificate. Under PERMISSIVE mTLS the sidecar also accepts plaintext, and a
// plaintext connection has no principal: the compiled AuthorizationPolicy
// still reads "only the hop", while anything that can reach the port is not
// held to it. The edges are therefore only enforceable where the namespace is
// STRICT, and the render has no business delivering the one without the other.
//
// So on a meshed environment — one that declares a host — the render writes
// the namespace-wide PeerAuthentication (pkg/routing NamespaceMeshPolicy:
// no selector, so every workload of the namespace, including pods this render
// never saw) as its own promotable layer, `mesh/overlays/<environment>/`,
// beside the module's units. It is a layer rather than a file inside a unit
// for the reason service_namespace.go gives for the Namespace object: the
// policy is namespace-scoped and one object, so two units shipping it would be
// two Argo Applications claiming it. It is its own layer rather than the
// module's bootstrap or manifest overlay because neither is always there — a
// module with no kustomize tree stages no bootstrap, and a render with no
// instance writes no manifest — and a guarantee that is present only sometimes
// is not one.
//
// checkMeshPosture is the other half: the render reads its own effective
// output back and refuses to deliver a meshed tree that does not carry the
// policy, at STRICT, for the namespace it deploys into. The projection and the
// check are deliberately separate — the check holds whatever reached the tree,
// including an agent's own PeerAuthentication at PERMISSIVE, which it refuses
// by name.

const (
	// meshPostureDir is the render subdirectory holding the namespace's mesh
	// posture, laid out like every other promotable layer.
	meshPostureDir = "mesh"
	// meshPostureFile is the namespace-wide STRICT policy's file.
	meshPostureFile = "peer-authentication.yaml"
)

// projectMeshPosture writes the namespace-wide STRICT mTLS policy for the
// namespace this render deploys into and returns the layer's tree-relative
// path, or "" when the environment declares no host and there is no mesh to
// make strict.
//
// A meshed render with no resolved namespace is refused: the policy is
// namespace-scoped, and a module whose namespace is unknown has no namespace
// to make strict.
func projectMeshPosture(tree *renderRoot, owned string, opts *RenderOptions) (string, error) {
	if opts.Host == nil {
		return "", nil
	}
	if opts.Namespace == "" {
		return "", fmt.Errorf("environment %s declares a host, so its mesh carries every call, but module %s has no resolved namespace to require STRICT mTLS in", opts.Environment, opts.Module)
	}
	overlay := filepath.Join(owned, meshPostureDir, "overlays", opts.Environment)
	if err := tree.mkdirAll(overlay); err != nil {
		return "", fmt.Errorf("create the mesh posture layer: %w", err)
	}
	if err := writeMeshPostureKustomizationInTree(tree, overlay); err != nil {
		return "", err
	}
	if err := writeOverlayResourceInTree(tree, overlay, meshPostureFile, routing.NamespaceMeshPolicy(opts.Namespace)); err != nil {
		return "", err
	}
	return filepath.ToSlash(meshPostureDir), nil
}

// checkMeshPosture refuses a render for a meshed environment whose effective
// manifests do not require STRICT mTLS across the namespace it deploys into,
// and refuses any PeerAuthentication in the tree that weakens it.
//
// It reads the EFFECTIVE manifests — the kustomize output validateTree already
// built — so an overlay patch that relaxed the mode, or an agent's own policy,
// is judged as the cluster would see it, not as the file was written.
func checkMeshPosture(manifests []manifest, opts *RenderOptions) error {
	if opts.Host == nil {
		return nil
	}
	var strict bool
	for _, item := range manifests {
		if item.group != routing.PeerAuthenticationGroup || item.kind != routing.PeerAuthenticationKind {
			continue
		}
		name := metadataString(item.value, keyName)
		mode, _ := mapField(mapField(item.value, "spec"), "mtls")["mode"].(string)
		if mode != routing.MeshModeSTRICT {
			return fmt.Errorf("%s declares %s %s with mtls.mode %q on environment %s: the mesh carries every call there, and a principal is only a fact about a connection the peer authenticated, so every policy of the namespace is %s",
				item.path, routing.PeerAuthenticationKind, name, mode, opts.Environment, routing.MeshModeSTRICT)
		}
		// The namespace-wide one is the guarantee: no selector, so every
		// workload of the namespace, whoever rendered it.
		if name == routing.MeshNamespacePolicyName && mapField(mapField(item.value, "spec"), "selector") == nil {
			strict = true
		}
	}
	if !strict {
		return fmt.Errorf("environment %s declares a host, so the render delivers mesh edges naming the principals admitted to each endpoint, but the tree carries no namespace-wide %s %q at mtls.mode %s for namespace %s: without it the mesh accepts plaintext, a plaintext peer has no principal, and the edges admit whatever can reach the port",
			opts.Environment, routing.PeerAuthenticationKind, routing.MeshNamespacePolicyName, routing.MeshModeSTRICT, opts.Namespace)
	}
	return nil
}

// writeMeshPostureKustomizationInTree writes the layer's empty kustomization,
// which writeOverlayResourceInTree then adds the policy to, so the layer is
// built by kustomize exactly as every other promoted overlay is.
func writeMeshPostureKustomizationInTree(tree *renderRoot, overlay string) error {
	encoded, err := yaml.Marshal(kustomizationManifest{
		APIVersion: kustomizeAPIVersion,
		Kind:       kindKustomization,
		Resources:  []string{},
	})
	if err != nil {
		return err
	}
	return tree.write(overlay, kustomizationFile, encoded)
}
