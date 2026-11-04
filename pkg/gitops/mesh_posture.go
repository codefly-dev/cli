package gitops

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

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
//
// IT RUNS ON EVERY PROMOTABLE PASS, NOT ONLY THE PRODUCING RENDER. It is
// reached from validateTree, so the verifiers reach it too: a tree rendered by
// a CLI that wrote no mesh layer, or one whose policy was edited to PERMISSIVE
// after the render, was otherwise verified, snapshotted, promoted and observed
// without complaint — every reader trusted the producer for the one guarantee
// that makes the delivered edges mean anything. A render records the posture it
// delivered in the inventory (MeshPosture), and a verifier requires it there
// whenever the inventory carries mesh edges, so the requirement travels with
// the tree instead of living in the environment block only the producer reads.
//
// The posture the render requires is STRICT and nothing else: PERMISSIVE is
// not a weaker setting of the same guarantee, it is the absence of it. A
// namespace that cannot yet be STRICT is a namespace whose edges are not
// enforceable, and the honest delivery is no edges, not edges with a note.

const (
	// meshPostureDir is the render subdirectory holding the namespace's mesh
	// posture, laid out like every other promotable layer.
	meshPostureDir = "mesh"
	// meshPostureFile is the namespace-wide STRICT policy's file.
	meshPostureFile = "peer-authentication.yaml"
)

// projectMeshPosture writes the namespace-wide STRICT mTLS policy for the
// namespace this render deploys into and returns the layer's tree-relative
// path and the posture it requires, or "" for both when the environment
// declares no host and there is no mesh to make strict.
//
// A meshed render with no resolved namespace is refused: the policy is
// namespace-scoped, and a module whose namespace is unknown has no namespace
// to make strict.
func projectMeshPosture(tree *renderRoot, owned string, opts *RenderOptions) (path, posture string, err error) {
	if opts.Host == nil {
		return "", "", nil
	}
	if opts.Namespace == "" {
		return "", "", fmt.Errorf("environment %s declares a host, so its mesh carries every call, but module %s has no resolved namespace to require STRICT mTLS in", opts.Environment, opts.Module)
	}
	overlay := filepath.Join(owned, meshPostureDir, "overlays", opts.Environment)
	if err = tree.mkdirAll(overlay); err != nil {
		return "", "", fmt.Errorf("create the mesh posture layer: %w", err)
	}
	if err = writeMeshPostureKustomizationInTree(tree, overlay); err != nil {
		return "", "", err
	}
	if err = writeOverlayResourceInTree(tree, overlay, meshPostureFile, routing.NamespaceMeshPolicy(opts.Namespace)); err != nil {
		return "", "", err
	}
	return filepath.ToSlash(meshPostureDir), routing.MeshModeSTRICT, nil
}

// meshPostureRequired reports whether this pass must find the namespace's
// STRICT policy in the tree: a render of an environment that declares a host,
// or a verifier reading a tree whose own record says it is meshed — the
// recorded posture, or the mesh edges whose enforceability it is.
//
// A tree carrying edges with NO recorded posture is the pre-#1280 render (or
// one whose record was edited to drop it), and it is required all the same:
// the check below then refuses it for the policy it does not carry, which is
// the honest answer either way.
func meshPostureRequired(opts *RenderOptions) bool {
	return opts.Host != nil || opts.MeshPosture != "" || opts.MeshEdges != nil
}

// checkMeshPosture refuses a render for a meshed environment whose effective
// manifests do not require STRICT mTLS across the namespace it deploys into,
// and refuses any PeerAuthentication in the tree that weakens it.
//
// It reads the EFFECTIVE manifests — the kustomize output validateTree already
// built — so an overlay patch that relaxed the mode, or an agent's own policy,
// is judged as the cluster would see it, not as the file was written.
func checkMeshPosture(manifests []manifest, opts *RenderOptions) error {
	if !meshPostureRequired(opts) {
		return nil
	}
	if opts.MeshEdges != nil && opts.MeshPosture == "" && opts.Host == nil {
		return fmt.Errorf("the render record of environment %s carries mesh edges but no delivered mesh posture (%q): the edges name the principals admitted to each endpoint, and they are only enforceable where the namespace requires %s mTLS, so a tree that delivers the one without the other is refused — re-render it with a CLI that writes the %s/ layer",
			opts.Environment, "meshPosture", routing.MeshModeSTRICT, meshPostureDir)
	}
	var strict bool
	for _, item := range manifests {
		if item.group != routing.PeerAuthenticationGroup || item.kind != routing.PeerAuthenticationKind {
			continue
		}
		name := metadataString(item.value, keyName)
		spec := mapField(item.value, "spec")
		mode, _ := mapField(spec, "mtls")["mode"].(string)
		if mode != routing.MeshModeSTRICT {
			return fmt.Errorf("%s declares %s %s with mtls.mode %q on environment %s: the mesh carries every call there, and a principal is only a fact about a connection the peer authenticated, so every policy of the namespace is %s",
				item.path, routing.PeerAuthenticationKind, name, mode, opts.Environment, routing.MeshModeSTRICT)
		}
		// AND NOT PER PORT. Istio takes spec.portLevelMtls, a mode per
		// workload port, and a port-level setting OVERRIDES the
		// namespace-wide STRICT for that port. So a policy that reads STRICT
		// at spec.mtls.mode and carries `portLevelMtls: {8081: {mode:
		// DISABLE}}` leaves 8081 accepting plaintext — with no peer
		// principal — while the compiled edges for that port still read
		// "only the hop". The mode is read, not the shape: an entry is
		// refused unless it is STRICT too, which makes a redundant entry
		// harmless and an exception impossible.
		if err := checkPortLevelMTLS(item, name, spec, opts); err != nil {
			return err
		}
		// The namespace-wide one is the guarantee: no selector, so every
		// workload of the namespace, whoever rendered it.
		if name == routing.MeshNamespacePolicyName && mapField(spec, "selector") == nil {
			strict = true
		}
	}
	if !strict {
		return fmt.Errorf("environment %s is meshed, so the render delivers mesh edges naming the principals admitted to each endpoint, but the tree carries no namespace-wide %s %q at mtls.mode %s for namespace %s: without it the mesh accepts plaintext, a plaintext peer has no principal, and the edges admit whatever can reach the port",
			opts.Environment, routing.PeerAuthenticationKind, routing.MeshNamespacePolicyName, routing.MeshModeSTRICT, opts.Namespace)
	}
	return nil
}

// checkPortLevelMTLS refuses any portLevelMtls entry that is not STRICT.
//
// The entries are keyed by PORT, and a port is a number: written unquoted
// (`8081:`, as Istio's own examples do) a YAML mapping with non-string keys
// decodes to map[any]any, not map[string]any, so reading it the way every
// other field here is read would find nothing and pass the policy. Both shapes
// are read, and a portLevelMtls that is present but is no mapping at all is
// refused rather than skipped: this check exists because what it cannot read it
// must not wave through.
func checkPortLevelMTLS(item manifest, name string, spec map[string]any, opts *RenderOptions) error {
	declared, present := spec["portLevelMtls"]
	if !present || declared == nil {
		return nil
	}
	entries := map[string]any{}
	switch typed := declared.(type) {
	case map[string]any:
		entries = typed
	case map[any]any:
		for key, value := range typed {
			entries[fmt.Sprintf("%v", key)] = value
		}
	default:
		return fmt.Errorf("%s declares %s %s with a portLevelMtls that is not a mapping of port to mode (%T) on environment %s: a per-port mode overrides the namespace's %s, so one this render cannot read is refused rather than trusted",
			item.path, routing.PeerAuthenticationKind, name, declared, opts.Environment, routing.MeshModeSTRICT)
	}
	for _, port := range slices.Sorted(maps.Keys(entries)) {
		setting, ok := entries[port].(map[string]any)
		if !ok {
			return fmt.Errorf("%s declares %s %s with portLevelMtls port %s that is not a mode setting (%T) on environment %s: a per-port mode overrides the namespace's %s, so one this render cannot read is refused rather than trusted",
				item.path, routing.PeerAuthenticationKind, name, port, entries[port], opts.Environment, routing.MeshModeSTRICT)
		}
		mode, _ := setting["mode"].(string)
		if mode == routing.MeshModeSTRICT {
			continue
		}
		return fmt.Errorf("%s declares %s %s with portLevelMtls port %s at mtls.mode %q on environment %s: a port-level mode overrides the namespace-wide %s, so that port accepts plaintext, a plaintext peer carries no principal, and the edges delivered for it admit whatever can reach it — every port of every policy of the namespace is %s",
			item.path, routing.PeerAuthenticationKind, name, port, mode, opts.Environment, routing.MeshModeSTRICT, routing.MeshModeSTRICT)
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
