package gitops

import (
	"fmt"
	"os"
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
// explicit audience, as a container mounts it.
type tokenMount struct {
	volume   string
	audience string
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
		workload := renderedWorkload{Name: metadataString(item.value, "name"), Kind: item.kind, tokenMounts: map[string][]tokenMount{}}
		workload.ServiceAccount, _ = spec["serviceAccountName"].(string)
		if workload.ServiceAccount == "" {
			workload.ServiceAccount = defaultServiceAccount
		}
		tokens := audienceTokenVolumes(sliceField(spec, "volumes"))
		containers, err := renderedContainers(workload.Name, sliceField(spec, "containers"), tokens, workload.tokenMounts)
		if err != nil {
			return nil, err
		}
		workload.Containers = containers
		if workload.InitContainers, err = renderedContainers(workload.Name, sliceField(spec, "initContainers"), tokens, workload.tokenMounts); err != nil {
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
func audienceTokenVolumes(raw []any) map[string]string {
	audiences := map[string]string{}
	for _, entry := range raw {
		volume, _ := entry.(map[string]any)
		name, _ := volume["name"].(string)
		for _, source := range sliceField(mapField(volume, "projected"), "sources") {
			projected, _ := source.(map[string]any)
			if audience, _ := mapField(projected, "serviceAccountToken")["audience"].(string); audience != "" && name != "" {
				audiences[name] = audience
			}
		}
	}
	return audiences
}

// renderedContainers reads a pod template's containers, their pinned images
// and the audience-bearing token volumes each mounts.
func renderedContainers(workload string, raw []any, tokens map[string]string, mounts map[string][]tokenMount) ([]renderedContainer, error) {
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
			if audience, carries := tokens[volume]; carries {
				mounts[name] = append(mounts[name], tokenMount{volume: volume, audience: audience})
			}
		}
		containers = append(containers, rendered)
	}
	return containers, nil
}

// overlayManifests reads the manifests of a unit's overlay: the kustomize build
// when the overlay has a kustomization, the YAML files themselves otherwise. A
// unit an agent rendered always has one; a tree assembled directly in a test
// may not, and what it says about its workloads is the same either way.
func overlayManifests(overlay string) ([]manifest, error) {
	for _, name := range []string{kustomizationFile, kustomizationFileAlt} {
		if _, err := os.Stat(filepath.Join(overlay, name)); err == nil {
			kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
			built, buildErr := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
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
	entries, err := os.ReadDir(overlay)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", overlay, err)
	}
	var manifests []manifest
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), yamlExtension) && !strings.HasSuffix(entry.Name(), ymlExtension)) {
			continue
		}
		data, readErr := readWithin(overlay, entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		decoded, _, decodeErr := decodeYAML(filepath.ToSlash(filepath.Join(overlay, entry.Name())), data)
		if decodeErr != nil {
			return nil, decodeErr
		}
		manifests = append(manifests, decoded...)
	}
	return manifests, nil
}
