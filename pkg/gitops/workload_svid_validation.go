package gitops

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

type workloadIdentityCheck struct {
	path        string
	service     *resources.Service
	environment *environments.Environment
	scope       unitScope
}

// Judge the final overlay, including authored patches targeting resources added
// by the projection. The expected data comes from the composition, never from a
// generated file that Kustomize can transform. This is part of the service
// configuration validation that must succeed before the tree can be promoted.
func validateProjectedWorkloadSVID(root string, service *resources.Service, env *environments.Environment, scope unitScope) error {
	unique := resources.ServiceUnique(scope.Module, service.Name)
	if _, exists := scope.WorkloadPeers[unique]; !exists && !requestsWorkloadIdentity(service) {
		return nil
	}
	documents, err := overlayManifests(filepath.Join(root, "overlays", env.Name))
	if err != nil {
		return err
	}
	return validateWorkloadSVIDManifests(documents, service, env, scope)
}

// Use validateTree's effective set, not another build of a nested overlay that
// could miss a transformation made by an enclosing Kustomization.
func validateWorkloadIdentityChecks(documents []manifest, checks []workloadIdentityCheck) error {
	for _, check := range checks {
		var unit []manifest
		for _, doc := range documents {
			if strings.HasPrefix(strings.TrimPrefix(doc.path, "kustomize:"), check.path+"/") {
				unit = append(unit, doc)
			}
		}
		if err := validateWorkloadSVIDManifests(unit, check.service, check.environment, check.scope); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkloadSVIDManifests(documents []manifest, service *resources.Service, env *environments.Environment, scope unitScope) error {
	unique := resources.ServiceUnique(scope.Module, service.Name)
	peer, exists := scope.WorkloadPeers[unique]
	if !requestsWorkloadIdentity(service) {
		if !exists {
			return nil
		}
		workloads, err := workloadsFromManifests(documents)
		if err != nil {
			return err
		}
		return verifyGatewayWorkloadPlan(workloads, unique, peer, env, scope.Namespace)
	}
	if !exists || !peer.Consumer {
		return fmt.Errorf("workload-identity service %s has no composition identity plan", unique)
	}
	files, err := scope.WorkloadPeers.files(env.Host, unique)
	if err != nil {
		return err
	}
	if err = validateWorkloadPeerFiles(documents, service.Name, scope.Namespace, files); err != nil {
		return err
	}
	workloads, err := workloadsFromManifests(documents)
	if err != nil {
		return err
	}
	values, err := workloadIdentityEnvironment(env.Name)
	if err != nil {
		return err
	}
	count := 0
	for i := range workloads {
		workload := &workloads[i]
		if !servingKind(workload.Kind) {
			continue
		}
		count++
		if err := verifyWorkloadPeer(workload, peer, env.Host, scope.Namespace); err != nil {
			return err
		}
		container, _, err := authenticatingContainer(service.Name, workload)
		if err != nil {
			return fmt.Errorf("%s %s: %w", workload.Kind, workload.Name, err)
		}
		provided, err := providesIdentityToken(workload, container.Name, env.Host.Audience)
		if err != nil || !provided {
			return fmt.Errorf("%s %s differs from the workload-identity token plan: provided=%t, error=%v", workload.Kind, workload.Name, provided, err)
		}
		for _, doc := range documents {
			if doc.kind == workload.Kind && metadataString(doc.value, keyName) == workload.Name {
				if err := validateWorkloadSVIDBindings(doc, service.Name, container.Name, scope.Namespace, values); err != nil {
					return fmt.Errorf("%s %s: %w", doc.kind, workload.Name, err)
				}
			}
		}
	}
	if count == 0 {
		return fmt.Errorf("workload-identity service %s renders no serving workload", unique)
	}
	return nil
}

func validateWorkloadPeerFiles(documents []manifest, service, namespace string, files map[string]string) error {
	name := service + "-workload-peers"
	expected := make(map[string]any, len(files))
	for key, value := range files {
		expected[key] = value
	}
	count := 0
	for _, doc := range documents {
		if doc.kind != kindConfigMap || metadataString(doc.value, keyName) != name {
			continue
		}
		count++
		if doc.group != "" || metadataString(doc.value, keyNamespace) != namespace ||
			!reflect.DeepEqual(mapField(doc.value, dataKey), expected) || len(mapField(doc.value, "binaryData")) != 0 {
			return fmt.Errorf("ConfigMap %s/%s differs from the composition workload peer plan", namespace, name)
		}
	}
	if count != 1 {
		return fmt.Errorf("ConfigMap %s/%s must occur exactly once in the effective workload peer plan", namespace, name)
	}
	return nil
}

func validateWorkloadSVIDBindings(doc manifest, service, authenticating, namespace string, values map[string]string) error {
	if metadataString(doc.value, keyNamespace) != namespace {
		return fmt.Errorf("namespace differs from workload-identity plan %s", namespace)
	}
	spec, _ := podSpec(doc)
	for _, volume := range []map[string]any{workloadSVIDVolume(), workloadPeersVolume(service)} {
		if err := requireIdentityEntry(spec, "volumes", volume); err != nil {
			return err
		}
	}
	for _, field := range []string{keyContainers, "initContainers", "ephemeralContainers"} {
		for _, raw := range sliceField(spec, field) {
			container, _ := raw.(map[string]any)
			chosen := field == keyContainers && container[keyName] == authenticating
			if err := validateWorkloadSVIDMounts(container, chosen); err != nil {
				return err
			}
			if chosen {
				for name, value := range values {
					if err := requireIdentityEntry(container, "env", map[string]any{keyName: name, "value": value}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateWorkloadSVIDMounts(container map[string]any, chosen bool) error {
	for _, reserved := range []struct{ name, mount string }{{svidVolume, svidMount}, {peersVolume, peersMount}} {
		expected := readOnlyMount(reserved.name, reserved.mount)
		for _, raw := range sliceField(container, "volumeMounts") {
			mount, _ := raw.(map[string]any)
			mountPath, _ := mount["mountPath"].(string)
			if mount[keyName] != reserved.name && (!chosen || !mountOverlaps(mountPath, reserved.mount)) {
				continue
			}
			if !chosen || !reflect.DeepEqual(mount, expected) {
				return fmt.Errorf("container %s mount %v differs from workload-identity plan for %s", container[keyName], mount[keyName], reserved.name)
			}
		}
		if chosen {
			if err := requireIdentityEntry(container, "volumeMounts", expected); err != nil {
				return err
			}
		}
	}
	return nil
}

// Compare the entire binding: items, optional, subPath, valueFrom or a duplicate
// name can redirect a seemingly correct ConfigMap, volume, mount or env value.
func requireIdentityEntry(owner map[string]any, field string, expected map[string]any) error {
	count := 0
	for _, raw := range sliceField(owner, field) {
		entry, _ := raw.(map[string]any)
		if entry[keyName] != expected[keyName] {
			continue
		}
		count++
		if !reflect.DeepEqual(entry, expected) {
			return fmt.Errorf("%s %s differs from the workload-identity plan", field, expected[keyName])
		}
	}
	if count != 1 {
		return fmt.Errorf("%s %s must occur exactly once in the workload-identity plan", field, expected[keyName])
	}
	return nil
}
