package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- What the render projects into the pods a host admits ---
//
// Two projections, each into the one container that authenticates and no
// other (the rule refuseSharedTokens holds the tree to):
//
//   - every serving workload a presence document declares gets a projected
//     ServiceAccount token minted for the host's audience. It is how the pod
//     proves who it is when it mints (the host reviews the token and reads
//     the pod's running image — module-saas-starter#1056): nothing else
//     carries a module's identity, and no secret or prefix is projected.
//   - on a k3d environment that declares the host's trust-anchor path, the
//     host's verifier (the service host.delivery names) gets the local trust
//     anchor mounted there: one projected volume, the local trust ConfigMap
//     (verification_policy.json) and Secret (trusted_root.json) as its only
//     sources, keys not remapped, read-only — the shape a cell's admission
//     pins the verifier to. A local cluster run provisions the two objects.
//
// Both land as patches in the unit's environment overlay, so the base the
// agent rendered is untouched and the overlay's bytes — which the presence
// document digests — are what runs.

const (
	// identityTokenVolume, identityTokenMount and identityTokenFile are where
	// the projected identity token is: /var/run/secrets/codefly/token, the
	// path the SDK's mint client reads (sdk-go workcontext).
	identityTokenVolume = "codefly-workload-identity"
	identityTokenMount  = "/var/run/secrets/codefly" //nolint:gosec // G101: a mount path, not a credential
	identityTokenFile   = "token"
	// identityTokenSeconds is the token's lifetime; the kubelet rotates it at
	// 80% and the SDK re-reads the file before every mint.
	identityTokenSeconds = 3600

	// LocalTrustAnchorObjects names the ConfigMap (verification_policy.json)
	// and the Secret (trusted_root.json) a local cluster run provisions in the
	// verifier's namespace, and the render mounts into the verifier.
	LocalTrustAnchorObjects = "codefly-delivery-trust"

	// The manifest keys a projection writes and reads.
	keyName                = "name"
	keyKind                = "kind"
	keySpec                = "spec"
	keyAPIVersion          = "apiVersion"
	LocalTrustAnchorVolume = "codefly-delivery-trust"
)

// projectWorkloadIdentity writes both projections into the staged tree.
func projectWorkloadIdentity(owned string, opts *RenderOptions) error {
	if opts.Host == nil {
		return nil
	}
	for index := range opts.SolutionInstances {
		for _, unit := range opts.SolutionInstances[index].Units {
			if err := projectIdentityToken(owned, opts, unit); err != nil {
				return fmt.Errorf("%s %s artifact %s: %w", opts.SolutionInstances[index].Kind, opts.SolutionInstances[index].Name, unit.Name, err)
			}
		}
	}
	return projectTrustAnchor(owned, opts)
}

// projectIdentityToken projects the host-audience token into the
// authenticating container of each serving workload of the unit, unless that
// container already mounts one for the audience.
func projectIdentityToken(owned string, opts *RenderOptions, unit SolutionArtifactUnit) error {
	unitDir := filepath.Join(owned, filepath.FromSlash(unit.Path))
	workloads, err := renderedWorkloads(unitDir, opts.Environment)
	if err != nil {
		return err
	}
	var patches []podProjection
	for index := range workloads {
		workload := &workloads[index]
		if !servingKind(workload.Kind) {
			continue
		}
		container, _, err := authenticatingContainer(unit.Name, workload)
		if err != nil {
			return fmt.Errorf("workload %s: %w", workload.Name, err)
		}
		provided, err := providesIdentityToken(workload, container.Name, opts.Host.Audience)
		if err != nil {
			return fmt.Errorf("workload %s: %w", workload.Name, err)
		}
		if provided {
			continue
		}
		volume := projectedVolume(identityTokenVolume, map[string]any{"serviceAccountToken": map[string]any{
			"audience": opts.Host.Audience, "expirationSeconds": identityTokenSeconds, "path": identityTokenFile,
		}})
		mount := readOnlyMount(identityTokenVolume, identityTokenMount)
		if workload.hasVolume(identityTokenVolume) {
			return fmt.Errorf("workload %s already declares a volume named %s, which the render projects; rename it", workload.Name, identityTokenVolume)
		}
		patches = append(patches, podProjection{kind: workload.Kind, name: workload.Name, container: container.Name, volume: volume, mount: mount})
	}
	return addOverlayPatches(unitDir, opts.Environment, patches)
}

// projectTrustAnchor mounts the local trust anchor into the verifier, on a
// k3d environment that declares the host's trust-anchor path.
func projectTrustAnchor(owned string, opts *RenderOptions) error {
	if !opts.Target.DeclaresLocalK3d() || opts.Host.TrustAnchor == "" {
		return nil
	}
	module, service, _ := opts.Host.DeliveryEndpoint()
	if module != opts.Module {
		return nil
	}
	for index := range opts.Units {
		unit := &opts.Units[index]
		if unit.Name != service || unit.Path == "" {
			continue
		}
		unitDir := filepath.Join(owned, filepath.FromSlash(unit.Path))
		workloads, err := renderedWorkloads(unitDir, opts.Environment)
		if err != nil {
			return err
		}
		var patches []podProjection
		for index := range workloads {
			workload := &workloads[index]
			if !servingKind(workload.Kind) {
				continue
			}
			container, _, err := authenticatingContainer(service, workload)
			if err != nil {
				return fmt.Errorf("the host's verifier %s workload %s: %w", service, workload.Name, err)
			}
			volume := projectedVolume(LocalTrustAnchorVolume,
				map[string]any{"configMap": map[string]any{keyName: LocalTrustAnchorObjects}},
				map[string]any{"secret": map[string]any{keyName: LocalTrustAnchorObjects}})
			mount := readOnlyMount(LocalTrustAnchorVolume, opts.Host.TrustAnchor)
			if workload.hasVolume(LocalTrustAnchorVolume) {
				return fmt.Errorf("the host's verifier workload %s already declares a volume named %s, which the render projects; rename it", workload.Name, LocalTrustAnchorVolume)
			}
			patches = append(patches, podProjection{kind: workload.Kind, name: workload.Name, container: container.Name, volume: volume, mount: mount})
		}
		if len(patches) == 0 {
			return fmt.Errorf("the host's verifier %s/%s renders no serving workload to mount the local trust anchor into", module, service)
		}
		return addOverlayPatches(unitDir, opts.Environment, patches)
	}
	return fmt.Errorf("the host's delivery API %s names service %s, which module %s does not render, so the local trust anchor has nowhere to be mounted", opts.Host.Delivery, service, module)
}

func servingKind(kind string) bool {
	return kind == kindDeployment || kind == kindStatefulSet || kind == kindDaemonSet
}

// providesIdentityToken reports whether the container already has the
// rotating host-audience token at exactly the path the SDK reads: a projected
// token for the audience, mounted without subPath (a subPath mount is never
// refreshed, so the token would stop rotating), landing at that file. A
// container whose mounts occupy that path otherwise — another volume at or
// above it, a token for another audience, a subPath mount — is refused with
// what it mounts there: adding ours beside it would shadow it or be shadowed.
func providesIdentityToken(workload *renderedWorkload, container, audience string) (bool, error) {
	required := path.Join(identityTokenMount, identityTokenFile)
	for _, mount := range workload.tokenMounts[container] {
		if mount.audience == audience && mount.file == required && !mount.subPathed {
			return true, nil
		}
	}
	for _, mounted := range workload.mountPaths[container] {
		cleaned := path.Clean(mounted)
		if cleaned == identityTokenMount || cleaned == required || strings.HasPrefix(identityTokenMount, cleaned+"/") || strings.HasPrefix(cleaned, identityTokenMount+"/") {
			return false, fmt.Errorf("container %s mounts something at %s, where the render projects the %q token the SDK reads at %s (a projected serviceAccountToken for audience %q at that file, mounted without subPath); mount it exactly so, or move what is there", container, mounted, audience, required, audience)
		}
	}
	return false, nil
}

// projectedVolume is a pod volume projecting the sources, keys not remapped.
func projectedVolume(name string, sources ...any) map[string]any {
	return map[string]any{keyName: name, "projected": map[string]any{"sources": sources}}
}

// readOnlyMount mounts the named volume read-only at path.
func readOnlyMount(name, path string) map[string]any {
	return map[string]any{keyName: name, "mountPath": path, "readOnly": true}
}

// podProjection is one volume added to a workload's pod and mounted into one
// of its containers.
type podProjection struct {
	kind, name, container string
	volume, mount         map[string]any
}

// patch is the projection as an inline strategic-merge patch.
func (projection *podProjection) patch() (map[string]any, error) {
	body := map[string]any{
		keyAPIVersion: "apps/v1",
		keyKind:       projection.kind,
		"metadata":    map[string]any{keyName: projection.name},
		keySpec: map[string]any{"template": map[string]any{keySpec: map[string]any{
			"volumes":    []any{projection.volume},
			"containers": []any{map[string]any{keyName: projection.container, "volumeMounts": []any{projection.mount}}},
		}}},
	}
	encoded, err := yaml.Marshal(body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"target": map[string]any{keyKind: projection.kind, keyName: projection.name}, "patch": string(encoded)}, nil
}

// applyTo adds the projection to a decoded manifest of its workload.
func (projection *podProjection) applyTo(document map[string]any) bool {
	if kind, _ := document[keyKind].(string); kind != projection.kind || metadataString(document, keyName) != projection.name {
		return false
	}
	spec := mapField(mapField(mapField(document, keySpec), "template"), keySpec)
	if spec == nil {
		return false
	}
	spec["volumes"] = append(sliceField(spec, "volumes"), projection.volume)
	for _, entry := range sliceField(spec, "containers") {
		if container, _ := entry.(map[string]any); container != nil && container[keyName] == projection.container {
			container["volumeMounts"] = append(sliceField(container, "volumeMounts"), projection.mount)
		}
	}
	return true
}

// addOverlayPatches writes the projections into the unit's environment
// overlay: as inline patches in its kustomization, or — an overlay of plain
// manifests, as a tree assembled without an agent is — into the manifests.
func addOverlayPatches(unitDir, environment string, projections []podProjection) error {
	if len(projections) == 0 {
		return nil
	}
	overlay := filepath.Join(unitDir, "overlays", environment)
	var path string
	for _, name := range []string{kustomizationFile, kustomizationFileAlt} {
		if _, err := os.Stat(filepath.Join(overlay, name)); err == nil {
			path = filepath.Join(overlay, name)
			break
		}
	}
	if path == "" {
		return projectIntoManifests(overlay, projections)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var kustomization map[string]any
	if err = yaml.Unmarshal(data, &kustomization); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if kustomization == nil {
		kustomization = map[string]any{}
	}
	patches, _ := kustomization["patches"].([]any)
	for index := range projections {
		patch, patchErr := projections[index].patch()
		if patchErr != nil {
			return patchErr
		}
		patches = append(patches, patch)
	}
	kustomization["patches"] = patches
	encoded, err := yaml.Marshal(kustomization)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

// projectIntoManifests applies the projections to the overlay's manifests.
func projectIntoManifests(overlay string, projections []podProjection) error {
	entries, err := os.ReadDir(overlay)
	if err != nil {
		return err
	}
	applied := make([]bool, len(projections))
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), yamlExtension) && !strings.HasSuffix(entry.Name(), ymlExtension)) {
			continue
		}
		path := filepath.Join(overlay, entry.Name())
		data, readErr := readWithin(overlay, entry.Name())
		if readErr != nil {
			return readErr
		}
		var documents []map[string]any
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var document map[string]any
			if decodeErr := decoder.Decode(&document); errors.Is(decodeErr, io.EOF) {
				break
			} else if decodeErr != nil {
				return fmt.Errorf("read %s: %w", path, decodeErr)
			}
			documents = append(documents, document)
		}
		changed := false
		for _, document := range documents {
			for index := range projections {
				if projections[index].applyTo(document) {
					applied[index], changed = true, true
				}
			}
		}
		if !changed {
			continue
		}
		var out bytes.Buffer
		encoder := yaml.NewEncoder(&out)
		for _, document := range documents {
			if encodeErr := encoder.Encode(document); encodeErr != nil {
				return encodeErr
			}
		}
		if err = encoder.Close(); err != nil {
			return err
		}
		if err = os.WriteFile(path, out.Bytes(), 0o600); err != nil {
			return err
		}
	}
	for index, done := range applied {
		if !done {
			return fmt.Errorf("overlay %s carries no manifest of %s %s to project into", overlay, projections[index].kind, projections[index].name)
		}
	}
	return nil
}

// hasVolume reports whether the workload's pod declares a volume of the name.
func (workload *renderedWorkload) hasVolume(name string) bool {
	for _, volume := range workload.volumes {
		if volume == name {
			return true
		}
	}
	return false
}
