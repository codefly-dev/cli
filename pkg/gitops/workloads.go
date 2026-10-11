package gitops

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// What the presence document reads off a rendered unit: every pod-producing
// workload of its overlay, the account each runs as, and every container's
// pinned image — read, never declared, so the document names the build the
// render pinned and the account the pod template names.

const (
	// defaultServiceAccount is the account a pod template naming none runs as:
	// shared by every such pod of the namespace, so never one presence names.
	defaultServiceAccount = "default"
)

// renderedWorkload is a workload as the render reads it off the rendered
// manifests, with the projected token volumes each container mounts, by
// container name, read so the render can refuse a token a sidecar could
// present.
type renderedWorkload struct {
	Name           string
	Kind           string
	ServiceAccount string
	Containers     []renderedContainer
	InitContainers []renderedContainer
	tokenMounts    map[string][]tokenMount
	volumes        []string
	// mounts are every container's volume mounts, by container name.
	mounts map[string][]containerMount
	// templateAnnotations are the pod template's annotations.
	templateAnnotations map[string]any
}

// renderedContainer is one container of a pod template and its pinned image.
type renderedContainer struct {
	Name  string
	Image renderedImage
}

// renderedImage is an image reference pinned by digest: the repository as
// written, without tag, and the digest.
type renderedImage struct {
	Repository string
	Digest     string
}

// tokenMount is one projected ServiceAccount token volume carrying an
// explicit audience, as a container mounts it: where the token file lands
// (the mount path joined with the projection's own path) and whether it is a
// subPath mount, which the kubelet never refreshes — a rotating token mounted
// that way stops rotating.
type tokenMount struct {
	volume    string
	audience  string
	file      string
	subPathed bool
	// lifetime is the projection's expirationSeconds; 0 when unset, which
	// the kubelet reads as 3600.
	lifetime int64
	// mountPath is where the volume is mounted.
	mountPath          string
	projectionConflict string
}

// containerMount is one volume mount of a container.
type containerMount struct {
	volume string
	path   string
}

// tokenSource is a projected token volume's audience and the path of the
// token file inside the volume.
type tokenSource struct {
	audience string
	path     string
	lifetime int64
	conflict string
}

// renderedWorkloads reads the pod-template-bearing workloads out of a unit's
// built overlay: their names, the account they run as, and their containers'
// pinned images.
func renderedWorkloads(unitDir, environment string) ([]renderedWorkload, error) {
	overlay := filepath.Join(unitDir, "overlays", environment)
	manifests, err := overlayManifests(overlay)
	if err != nil {
		return nil, err
	}
	return workloadsFromManifests(manifests)
}

func workloadsFromManifests(manifests []manifest) ([]renderedWorkload, error) {
	var workloads []renderedWorkload
	for _, item := range manifests {
		switch item.kind {
		case kindDeployment, kindStatefulSet, kindDaemonSet, kindJob, kindCronJob:
		default:
			continue
		}
		spec, ok := podSpec(item)
		if !ok {
			continue
		}
		workload := renderedWorkload{Name: metadataString(item.value, "name"), Kind: item.kind, tokenMounts: map[string][]tokenMount{}, mounts: map[string][]containerMount{}}
		workload.ServiceAccount, _ = spec["serviceAccountName"].(string)
		workload.templateAnnotations = mapField(mapField(mapField(item.value, "spec"), "template"), "metadata")
		workload.templateAnnotations = mapField(workload.templateAnnotations, "annotations")
		if workload.ServiceAccount == "" {
			workload.ServiceAccount = defaultServiceAccount
		}
		tokens := audienceTokenVolumes(sliceField(spec, "volumes"))
		for _, entry := range sliceField(spec, "volumes") {
			if volume, _ := entry.(map[string]any); volume != nil {
				if name, _ := volume["name"].(string); name != "" {
					workload.volumes = append(workload.volumes, name)
				}
			}
		}
		containers, err := renderedContainers(workload.Name, sliceField(spec, "containers"), tokens, workload.tokenMounts, workload.mounts)
		if err != nil {
			return nil, err
		}
		workload.Containers = containers
		if workload.InitContainers, err = renderedContainers(workload.Name, sliceField(spec, "initContainers"), tokens, workload.tokenMounts, workload.mounts); err != nil {
			return nil, err
		}
		workloads = append(workloads, workload)
	}
	return workloads, nil
}

// audienceTokenVolumes indexes a pod template's projected volumes that carry a
// ServiceAccount token minted for an explicit audience, by volume name. The
// token the ServiceAccount admission plugin injects (kube-api-access-…) is a
// projected token volume too, mounted into every container, and names no
// audience — keying on the audience is what tells the two apart, and it is
// what the presence document's refusal keys on (refuseSharedTokens).
func audienceTokenVolumes(raw []any) map[string][]tokenSource {
	audiences := map[string][]tokenSource{}
	for _, entry := range raw {
		volume, _ := entry.(map[string]any)
		name, _ := volume["name"].(string)
		sources := sliceField(mapField(volume, "projected"), "sources")
		for index, source := range sources {
			projected, _ := source.(map[string]any)
			token := mapField(projected, serviceAccountTokenProjection)
			if audience, _ := token["audience"].(string); audience != "" && name != "" {
				path, _ := token["path"].(string)
				audiences[name] = append(audiences[name], tokenSource{audience: audience, path: path, lifetime: integerField(token, "expirationSeconds"), conflict: projectedTokenConflict(sources, index, path)})
			}
		}
	}
	return audiences
}

// renderedContainers reads a pod template's containers, their pinned images
// and the audience-bearing token volumes each mounts.
func renderedContainers(workload string, raw []any, tokens map[string][]tokenSource, mounts map[string][]tokenMount, paths map[string][]containerMount) ([]renderedContainer, error) {
	var containers []renderedContainer
	for _, entry := range raw {
		container, _ := entry.(map[string]any)
		name, _ := container["name"].(string)
		image, _ := container["image"].(string)
		repository, digest, pinned := strings.Cut(image, "@")
		if !pinned {
			return nil, fmt.Errorf("workload %s container %s image %q is not pinned by digest", workload, name, image)
		}
		// The repository is the name only: a tag beside a digest would give one
		// container two answers about what it runs.
		if at := strings.LastIndex(repository, ":"); at > strings.LastIndex(repository, "/") {
			repository = repository[:at]
		}
		rendered := renderedContainer{Name: name, Image: renderedImage{Repository: repository, Digest: digest}}
		for _, raw := range sliceField(container, "volumeMounts") {
			mount, _ := raw.(map[string]any)
			volume, _ := mount["name"].(string)
			mountPath, _ := mount["mountPath"].(string)
			paths[name] = append(paths[name], containerMount{volume: volume, path: path.Clean(mountPath)})
			for _, source := range tokens[volume] {
				subPath, _ := mount["subPath"].(string)
				subPathExpr, _ := mount["subPathExpr"].(string)
				// A subPath mount presents the part of the volume it names AT
				// the mount path: the token file lands at the mount path when
				// the subPath is the token's own path.
				file := path.Join(mountPath, source.path)
				if subPath != "" {
					file = path.Join(mountPath, strings.TrimPrefix(strings.TrimPrefix(source.path, subPath), "/"))
				}
				mounts[name] = append(mounts[name], tokenMount{
					volume: volume, audience: source.audience, file: file, lifetime: source.lifetime, mountPath: path.Clean(mountPath),
					subPathed: subPath != "" || subPathExpr != "", projectionConflict: source.conflict,
				})
			}
		}
		containers = append(containers, rendered)
	}
	return containers, nil
}

// Keep the staged root while selecting a unit. Libraries see a checked tree,
// never a root opened on an unchecked service path.
func renderedWorkloadsInTree(tree *renderRoot, unitDir, environment string) ([]renderedWorkload, error) {
	overlay, err := renderOverlay(tree, unitDir, environment)
	if err != nil {
		return nil, err
	}
	manifests, err := overlayManifestsInTree(tree, overlay)
	if err != nil {
		return nil, err
	}
	return workloadsFromManifests(manifests)
}

func overlayManifestsInTree(tree *renderRoot, overlay string) ([]manifest, error) {
	checked, err := tree.checkedPath(overlay)
	if err != nil {
		return nil, err
	}
	filesystem, err := tree.fileSystem()
	if err != nil {
		return nil, err
	}
	return overlayManifestsOnFS(checked, filesystem)
}

// overlayManifests reads the manifests of a unit's overlay: the kustomize build
// when the overlay has a kustomization, the YAML files themselves otherwise. A
// unit an agent rendered always has one; a tree assembled directly in a test
// may not, and what it says about its workloads is the same either way.
func overlayManifests(overlay string) ([]manifest, error) {
	return overlayManifestsOnFS(overlay, filesys.MakeFsOnDisk())
}

func overlayManifestsOnFS(overlay string, filesystem filesys.FileSystem) ([]manifest, error) {
	for _, name := range []string{kustomizationFile, kustomizationFileAlt, kindKustomization} {
		if filesystem.Exists(filepath.Join(overlay, name)) {
			kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
			built, buildErr := kustomizer.Run(filesystem, overlay)
			if buildErr != nil {
				return nil, fmt.Errorf("build %s: %w", overlay, buildErr)
			}
			output, encodeErr := built.AsYaml()
			if encodeErr != nil {
				return nil, fmt.Errorf("encode %s: %w", overlay, encodeErr)
			}
			manifests, _, decodeErr := decodeYAML("kustomize:"+filepath.ToSlash(overlay), output)
			return manifests, decodeErr
		}
	}
	entries, err := filesystem.ReadDir(overlay)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", overlay, err)
	}
	var manifests []manifest
	for _, entry := range entries {
		if filesystem.IsDir(filepath.Join(overlay, entry)) || (!strings.HasSuffix(entry, yamlExtension) && !strings.HasSuffix(entry, ymlExtension)) {
			continue
		}
		data, readErr := filesystem.ReadFile(filepath.Join(overlay, entry))
		if readErr != nil {
			return nil, readErr
		}
		decoded, _, decodeErr := decodeYAML(filepath.ToSlash(filepath.Join(overlay, entry)), data)
		if decodeErr != nil {
			return nil, decodeErr
		}
		manifests = append(manifests, decoded...)
	}
	return manifests, nil
}

// integerField reads an integer a manifest decodes as int, int64, uint64 or
// float64, or 0 when absent or not a number.
func integerField(value map[string]any, key string) int64 {
	switch number := value[key].(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case uint64:
		if number > 1<<62 {
			return 1 << 62
		}
		return int64(number)
	case float64:
		return int64(number)
	}
	return 0
}
