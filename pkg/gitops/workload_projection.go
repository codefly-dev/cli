package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- What the render projects into the pods a host admits ---
//
// Two projections, each into the one container that authenticates and no
// other (the rule refuseSharedTokens holds the tree to):
//
//   - every identity workload of the module — each service declaring
//     module-identity, the workloads its manifest lists — gets a projected
//     ServiceAccount token minted for the host's audience, mounted ONLY into
//     the container the manifest declares for it. It is how the pod proves
//     who it is when it mints: the host reviews the token and nothing else,
//     so which container holds it is decided here, at deploy time, and a
//     sidecar never does. No secret or prefix is projected.
//   - the host's verifier (the service host.delivery names) gets its trust
//     anchor mounted: one projected volume, one ConfigMap
//     (verification_policy.json) as its only source, keys not remapped,
//     read-only. On a k3d environment that declares the host's trust-anchor
//     path it is the local trust ConfigMap a local cluster run provisions,
//     at that path. On any other environment that declares a host it is the
//     platform's `<host.component>-delivery-trust` ConfigMap at the host's
//     constant path, as one projected ConfigMap source — the shape the
//     platform's admission pins (obin-ai/infra-base#1366): the platform
//     renders that ConfigMap, the composition's Deployment carries the
//     mount, and this render is that Deployment. The same workloads, and
//     no other, mount their ServiceAccount's Kubernetes API credential
//     (automountServiceAccountToken: true): the host authorises every
//     delivered carrier by TokenReview, and reads the in-cluster CA and
//     token at the standard paths the ServiceAccount admission plugin
//     injects them at — nothing else. Every other workload keeps the
//     agents' default, false.
//
// Both land as patches in the unit's environment overlay, so the base the
// agent rendered is untouched and the overlay's bytes are what runs.

const (
	// identityTokenVolume, identityTokenMount and identityTokenFile are where
	// the projected identity token is: /var/run/secrets/codefly/token, the
	// path the SDK's mint client reads (sdk-go workcontext).
	identityTokenVolume = "codefly-workload-identity"
	identityTokenMount  = "/var/run/secrets/codefly" //nolint:gosec // G101: a mount path, not a credential
	identityTokenFile   = "token"
	// identityTokenSeconds is the token's lifetime: the longest a host admits
	// for a mint token (module-saas-starter#1068 refuses one spanning more
	// than 600 s, so a copied token is replayable for at most that long), and
	// the shortest the kubelet projects. It rotates at 80% and the SDK
	// re-reads the file before every mint.
	identityTokenSeconds = 600

	// LocalTrustAnchorObjects names the ConfigMap (verification_policy.json)
	// a local cluster run provisions in the verifier's namespace, and the
	// render mounts into the verifier.
	LocalTrustAnchorObjects = "codefly-delivery-trust"

	// HostedTrustAnchorPath is where a deployed host reads its trust anchor,
	// and the other half of the host's own constant
	// (module-saas-starter accounts `SolutionHostTrustAnchorPath`, which the
	// host refuses to relocate from inside a pod). It is never a workspace
	// value: the workspace environment is the composition's, so a
	// composition that could name the path could point the verifier at a
	// policy it wrote itself and sign its own documents against it. The
	// anchor stays reachable only by whoever mounts into the pod, which the
	// render does here at the one path, and workspace.go refuses
	// host.trust_anchor on every environment that is not a local k3d one.
	HostedTrustAnchorPath = "/etc/obin/delivery-trust"

	// hostedTrustAnchorSuffix completes the platform's name for the trust
	// ConfigMap it renders in the verifier's namespace,
	// `<host.component>-delivery-trust` (obin-ai/infra-base cell_contract.py,
	// `{cell.delivery}-delivery-trust`). The render projects it as one
	// ConfigMap source, no items, read-only, at the constant path — the shape
	// the platform's admission pins (obin-ai/infra-base#1366) — and declares
	// nothing about its content.
	hostedTrustAnchorSuffix = "-delivery-trust"

	// TrustAnchorAnnotation is the pod-template annotation a local cluster
	// run stamps with the trust anchor's digest. It is the run's, and the
	// render refuses a workload that carries it in desired state: a value
	// Argo CD owned would revert the stamp the revocation depends on.
	TrustAnchorAnnotation = "codefly.dev/trust-anchor"

	// The manifest keys a projection writes and reads.
	keyName           = "name"
	keyKind           = "kind"
	keySpec           = "spec"
	keyAPIVersion     = "apiVersion"
	keyMetadata       = "metadata"
	keyConfigMap      = "configMap"
	keyContainers     = "containers"
	keyNamespace      = "namespace"
	keyAutomountToken = "automountServiceAccountToken"
	// keyPatchTarget names the object a kustomize patch applies to.
	keyPatchTarget         = "target"
	keyPatch               = "patch"
	LocalTrustAnchorVolume = "codefly-delivery-trust"
)

// projectWorkloadIdentity writes both projections into the staged tree.
func projectWorkloadIdentity(tree *renderRoot, owned string, opts *RenderOptions) error {
	if opts.Host == nil {
		return nil
	}
	if err := refuseReservedAnnotation(tree, owned, opts); err != nil {
		return err
	}
	if instance := opts.Instance; instance != nil {
		for _, identity := range instance.Identities {
			if err := projectIdentityToken(tree, owned, opts, identity); err != nil {
				return fmt.Errorf("%s %s service %s: %w", instance.Kind, instance.Name, identity.Service, err)
			}
		}
	}
	return projectTrustAnchor(tree, owned, opts)
}

// refuseReservedAnnotation refuses a workload whose pod template carries the
// trust-anchor annotation in desired state: a local cluster run stamps it,
// and a value Argo CD owned would revert the stamp a revocation depends on.
func refuseReservedAnnotation(tree *renderRoot, owned string, opts *RenderOptions) error {
	for index := range opts.Units {
		unit := &opts.Units[index]
		if unit.Path == "" {
			continue
		}
		unitDir := filepath.Join(owned, filepath.FromSlash(unit.Path))
		if _, err := tree.stat(unitDir, filepath.Join("overlays", opts.Environment)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		workloads, err := renderedWorkloadsInTree(tree, unitDir, opts.Environment)
		if err != nil {
			return err
		}
		for index := range workloads {
			workload := &workloads[index]
			if _, reserved := workload.templateAnnotations[TrustAnchorAnnotation]; reserved {
				return fmt.Errorf("%s %s of %s carries the pod-template annotation %s, which a local cluster run stamps with its trust anchor's digest; remove it from the manifests", workload.Kind, workload.Name, unit.Name, TrustAnchorAnnotation)
			}
		}
	}
	return nil
}

// projectIdentityToken projects the host-audience token into the
// authenticating container of each serving workload of an identity service's
// unit, unless that container already mounts one for the audience.
func projectIdentityToken(tree *renderRoot, owned string, opts *RenderOptions, identity IdentityWorkload) error {
	unitDir := filepath.Join(owned, filepath.FromSlash(identity.Path))
	workloads, err := renderedWorkloadsInTree(tree, unitDir, opts.Environment)
	if err != nil {
		return err
	}
	var patches []*podProjection
	for index := range workloads {
		workload := &workloads[index]
		if !servingKind(workload.Kind) {
			continue
		}
		container, err := authenticatingContainer(identity.Service, workload)
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
		volume := projectedVolume(identityTokenVolume, map[string]any{serviceAccountTokenProjection: map[string]any{
			"audience": opts.Host.Audience, "expirationSeconds": identityTokenSeconds, "path": identityTokenFile,
		}})
		mount := readOnlyMount(identityTokenVolume, identityTokenMount)
		if workload.hasVolume(identityTokenVolume) {
			return fmt.Errorf("workload %s already declares a volume named %s, which the render projects; rename it", workload.Name, identityTokenVolume)
		}
		patches = append(patches, &podProjection{kind: workload.Kind, name: workload.Name, container: container.Name, volume: volume, mount: mount})
	}
	return addOverlayPatches(tree, unitDir, opts.Environment, patches)
}

// configMapNamePattern is a DNS-1123 subdomain, the shape a ConfigMap name
// takes, with configMapNameLimit its length; a host component they refuse
// cannot name the platform's trust ConfigMap.
var configMapNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

const configMapNameLimit = 253

// trustAnchorSource is the ConfigMap the verifier's trust anchor is projected
// from and the path it is mounted at, or empty when this render mounts none:
// a render with no target environment, or a k3d environment that states no
// trust-anchor path.
func trustAnchorSource(opts *RenderOptions) (configMap, mountPath string, err error) {
	switch {
	case opts.Target == nil:
		return "", "", nil
	case opts.Target.DeclaresLocalK3d():
		if opts.Host.TrustAnchor == "" {
			return "", "", nil
		}
		return LocalTrustAnchorObjects, opts.Host.TrustAnchor, nil
	case opts.Host.TrustAnchor != "":
		return "", "", fmt.Errorf("environment %s declares host.trust_anchor, which only a local cluster run mounts; a deployed host's trust anchor is its platform's, mounted at %s", opts.Target.Name, HostedTrustAnchorPath)
	}
	name := opts.Host.Component + hostedTrustAnchorSuffix
	if len(name) > configMapNameLimit || !configMapNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("environment %s declares host.component %q, which cannot name the platform's trust anchor ConfigMap %s: a ConfigMap name is a lowercase DNS subdomain of at most %d characters", opts.Target.Name, opts.Host.Component, name, configMapNameLimit)
	}
	return name, HostedTrustAnchorPath, nil
}

// projectTrustAnchor mounts the host's trust anchor into the verifier: the
// local trust ConfigMap at the declared path on a k3d environment, the
// platform's trust ConfigMap at the host's constant path on a hosted one.
// The same workloads mount their ServiceAccount's API credential, which the
// host's TokenReview of a delivered carrier needs and no other workload
// holds. Only the verifier's module is judged, so a host block the source
// refuses refuses that one render and no other module's.
func projectTrustAnchor(tree *renderRoot, owned string, opts *RenderOptions) error {
	module, service, _ := opts.Host.DeliveryEndpoint()
	if module != opts.Module {
		return nil
	}
	configMap, mountPath, err := trustAnchorSource(opts)
	if err != nil || configMap == "" {
		return err
	}
	for index := range opts.Units {
		unit := &opts.Units[index]
		if unit.Name != service || unit.Path == "" {
			continue
		}
		unitDir := filepath.Join(owned, filepath.FromSlash(unit.Path))
		workloads, err := renderedWorkloadsInTree(tree, unitDir, opts.Environment)
		if err != nil {
			return err
		}
		var patches []*podProjection
		for index := range workloads {
			workload := &workloads[index]
			if !servingKind(workload.Kind) {
				continue
			}
			container, err := authenticatingContainer(service, workload)
			if err != nil {
				return fmt.Errorf("the host's verifier %s workload %s: %w", service, workload.Name, err)
			}
			volume := projectedVolume(LocalTrustAnchorVolume,
				map[string]any{keyConfigMap: map[string]any{keyName: configMap}})
			mount := readOnlyMount(LocalTrustAnchorVolume, mountPath)
			if workload.hasVolume(LocalTrustAnchorVolume) {
				return fmt.Errorf("the host's verifier workload %s already declares a volume named %s, which the render projects; rename it", workload.Name, LocalTrustAnchorVolume)
			}
			patches = append(patches, &podProjection{kind: workload.Kind, name: workload.Name, container: container.Name, volume: volume, mount: mount, automountToken: true})
		}
		if len(patches) == 0 {
			return fmt.Errorf("the host's verifier %s/%s renders no serving workload to mount the trust anchor into", module, service)
		}
		return addOverlayPatches(tree, unitDir, opts.Environment, patches)
	}
	return fmt.Errorf("the host's delivery API %s names service %s, which module %s does not render, so the trust anchor has nowhere to be mounted", opts.Host.Delivery, service, module)
}

func servingKind(kind string) bool {
	return kind == kindDeployment || kind == kindStatefulSet || kind == kindDaemonSet
}

// authenticatingContainer is the container of a workload that authenticates
// for a service: the one named like the service, or the only one. A workload
// with several containers and none named like the service is refused rather
// than guessed — "whichever one presented the token" is how a sidecar ends up
// holding a workload's authority — and so is a pod template in which any
// other container mounts a token minted for an audience (refuseSharedTokens).
func authenticatingContainer(service string, workload *renderedWorkload) (renderedContainer, error) {
	var chosen *renderedContainer
	for index := range workload.Containers {
		if workload.Containers[index].Name == service {
			chosen = &workload.Containers[index]
			break
		}
	}
	if chosen == nil && len(workload.Containers) == 1 {
		chosen = &workload.Containers[0]
	}
	if chosen == nil {
		names := make([]string, 0, len(workload.Containers))
		for _, container := range workload.Containers {
			names = append(names, container.Name)
		}
		return renderedContainer{}, fmt.Errorf("none of its containers (%s) is named %q, so the one that authenticates cannot be told from a sidecar", strings.Join(names, ", "), service)
	}
	if err := refuseSharedTokens(chosen.Name, workload); err != nil {
		return renderedContainer{}, err
	}
	return *chosen, nil
}

// refuseSharedTokens refuses a pod template in which a projected token minted
// for an explicit audience is mounted by a container other than the one that
// authenticates — a sidecar or an init container, which would present it as
// the workload. The rule keys on the token's audience: the token the
// ServiceAccount plugin injects is projected too, mounted everywhere, and
// names none.
func refuseSharedTokens(authenticating string, workload *renderedWorkload) error {
	var shared []string
	for _, container := range append(append([]renderedContainer(nil), workload.Containers...), workload.InitContainers...) {
		if container.Name == authenticating {
			continue
		}
		for _, mount := range workload.tokenMounts[container.Name] {
			shared = append(shared, fmt.Sprintf("%s mounts %s (audience %q)", container.Name, mount.volume, mount.audience))
		}
	}
	if len(shared) == 0 {
		return nil
	}
	sort.Strings(shared)
	return fmt.Errorf("a projected token minted for an audience is mounted by a container other than the authenticating one, %q: %s; a token the pod shares is a token a sidecar can present as the workload, and the cell's admission refuses the pod for it, so mount it into %q alone",
		authenticating, strings.Join(shared, ", "), authenticating)
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
		if mount.audience != audience || mount.file != required || mount.subPathed {
			continue
		}
		if mount.projectionConflict != "" {
			return false, fmt.Errorf("container %s volume %s cannot reuse the identity token: %s", container, mount.volume, mount.projectionConflict)
		}
		// The workload's own projection is never kept when it lives
		// longer than the host admits: every mint from the pod would be
		// refused. Unset is the kubelet's 3600 s.
		if mount.lifetime == 0 || mount.lifetime > identityTokenSeconds {
			lifetime := "unset, which the kubelet projects for 3600 s"
			if mount.lifetime != 0 {
				lifetime = fmt.Sprintf("%d s", mount.lifetime)
			}
			return false, fmt.Errorf("container %s projects the %q token at %s with expirationSeconds %s, and a host refuses a mint token living longer than %d s; set expirationSeconds: %d on volume %s", container, audience, required, lifetime, identityTokenSeconds, identityTokenSeconds, mount.volume)
		}
		// Kept only when nothing else is mounted where it would hide the
		// token: at the same directory, at or below the token's path, or
		// between the projection's mount and the token.
		if shadow := shadowingMount(workload.mounts[container], mount.volume, mount.mountPath, required); shadow != nil {
			return false, fmt.Errorf("container %s projects the %q token at %s, and mounts volume %s at %s, which hides it; the SDK would read that instead — move it", container, audience, required, shadow.volume, shadow.path)
		}
		return true, nil
	}
	for _, mounted := range workload.mounts[container] {
		if mounted.path == identityTokenMount || mounted.path == required || strings.HasPrefix(identityTokenMount, mounted.path+"/") || strings.HasPrefix(mounted.path, identityTokenMount+"/") {
			return false, fmt.Errorf("container %s mounts volume %s at %s, where the render projects the %q token the SDK reads at %s (a projected serviceAccountToken for audience %q at that file, expirationSeconds %d, mounted without subPath); mount it exactly so, or move what is there", container, mounted.volume, mounted.path, audience, required, audience, identityTokenSeconds)
		}
	}
	return false, nil
}

// shadowingMount is a mount other than (volume at mountPath) that hides the
// file at required: one at the same directory, at or below the file, or
// between the directory and the file.
func shadowingMount(mounts []containerMount, volume, mountPath, required string) *containerMount {
	for index := range mounts {
		other := &mounts[index]
		if other.volume == volume && other.path == mountPath {
			continue
		}
		switch {
		case other.path == mountPath,
			other.path == required || strings.HasPrefix(other.path, required+"/"),
			strings.HasPrefix(other.path, mountPath+"/") && strings.HasPrefix(required, other.path+"/"):
			return other
		}
	}
	return nil
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
	// env binds CLI-owned file paths to the same container as their volume.
	env map[string]string
	// automountToken mounts the pod's ServiceAccount API credential at the
	// standard paths (automountServiceAccountToken: true), the one the
	// host's verifier reviews tokens with.
	automountToken bool
}

func (projection *podProjection) environmentEntries() []any {
	keys := make([]string, 0, len(projection.env))
	for key := range projection.env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]any, 0, len(keys))
	for _, key := range keys {
		// Clear a previous source in a strategic-merge patch: a path is a
		// literal, even when an authored group previously sourced this key.
		entries = append(entries, map[string]any{envEntryName: key, "value": projection.env[key], "valueFrom": nil})
	}
	return entries
}

// patch is the projection as an inline strategic-merge patch.
func (projection *podProjection) patch() (map[string]any, error) {
	container := map[string]any{keyName: projection.container, "volumeMounts": []any{projection.mount}}
	if len(projection.env) > 0 {
		container["env"] = projection.environmentEntries()
	}
	spec := map[string]any{"volumes": []any{projection.volume}, keyContainers: []any{container}}
	if projection.automountToken {
		spec[keyAutomountToken] = true
	}
	body := map[string]any{
		keyAPIVersion: "apps/v1",
		keyKind:       projection.kind,
		"metadata":    map[string]any{keyName: projection.name},
		keySpec:       map[string]any{"template": map[string]any{keySpec: spec}},
	}
	encoded, err := yaml.Marshal(body)
	if err != nil {
		return nil, err
	}
	return map[string]any{keyPatchTarget: map[string]any{keyKind: projection.kind, keyName: projection.name}, keyPatch: string(encoded)}, nil
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
	if projection.automountToken {
		spec[keyAutomountToken] = true
	}
	for _, entry := range sliceField(spec, "containers") {
		if container, _ := entry.(map[string]any); container != nil && container[keyName] == projection.container {
			container["volumeMounts"] = append(sliceField(container, "volumeMounts"), projection.mount)
			if len(projection.env) > 0 {
				var retained []any
				for _, raw := range sliceField(container, "env") {
					variable, _ := raw.(map[string]any)
					name, _ := variable[envEntryName].(string)
					if _, replaced := projection.env[name]; !replaced {
						retained = append(retained, raw)
					}
				}
				container["env"] = append(retained, projection.environmentEntries()...)
			}
		}
	}
	return true
}

// target names the workload the projection patches.
func (projection *podProjection) target() (kind, name string) {
	return projection.kind, projection.name
}

// overlayProjection is one change the render makes to an object of a unit's
// environment overlay: a pod projection, or a Service's labels.
type overlayProjection interface {
	patch() (map[string]any, error)
	applyTo(document map[string]any) bool
	target() (kind, name string)
}

// addOverlayPatches writes the projections into the unit's environment
// overlay: as inline patches in its kustomization, or — an overlay of plain
// manifests, as a tree assembled without an agent is — into the manifests.
func addOverlayPatches[P overlayProjection](tree *renderRoot, unitDir, environment string, projections []P) error {
	if len(projections) == 0 {
		return nil
	}
	overlay, err := renderOverlay(tree, unitDir, environment)
	if err != nil {
		return err
	}
	if err = tree.inspect(unitDir); err != nil {
		return err
	}
	var path string
	for _, name := range []string{kustomizationFile, kustomizationFileAlt, kindKustomization} {
		if _, statErr := tree.stat(overlay, name); statErr == nil {
			path = name
			break
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	if path == "" {
		return projectIntoManifests(tree, overlay, projections)
	}
	data, err := tree.read(overlay, path)
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
	return tree.write(overlay, path, encoded)
}

// projectIntoManifests applies the projections to the overlay's manifests.
func projectIntoManifests[P overlayProjection](tree *renderRoot, overlay string, projections []P) error {
	entries, err := tree.readDir(overlay)
	if err != nil {
		return err
	}
	applied := make([]bool, len(projections))
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), yamlExtension) && !strings.HasSuffix(entry.Name(), ymlExtension)) {
			continue
		}
		path := filepath.Join(overlay, entry.Name())
		data, readErr := tree.read(overlay, entry.Name())
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
		if err = tree.write(overlay, entry.Name(), out.Bytes()); err != nil {
			return err
		}
	}
	for index, done := range applied {
		if !done {
			kind, name := projections[index].target()
			return fmt.Errorf("overlay %s carries no manifest of %s %s to project into", overlay, kind, name)
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
