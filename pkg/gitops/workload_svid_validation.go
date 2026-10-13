package gitops

import (
	"errors"
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
	objects     []manifest
}

// Retain the service's own effective render before the tree is composed. These
// objects are independent decoded values, so later filesystem changes cannot
// revise what the service produced.
func (check *workloadIdentityCheck) capture(tree *renderRoot, root string) error {
	objects, err := check.currentObjects(tree, root)
	if err != nil {
		return err
	}
	check.objects = objects
	return nil
}

func (check *workloadIdentityCheck) currentObjects(tree *renderRoot, root string) ([]manifest, error) {
	overlay, err := serviceRenderOverlay(tree, filepath.Join(root, check.path), check.scope.Module, check.service.Name, check.environment.Name)
	if err != nil {
		return nil, err
	}
	documents, err := overlayManifestsInTree(tree, overlay)
	if err != nil {
		return nil, err
	}
	var owned []manifest
	for _, doc := range documents {
		if requestsWorkloadIdentity(check.service) && !identityOwnedObject(doc, check.service.Name) {
			continue
		}
		doc.path = strings.TrimPrefix(strings.TrimPrefix(doc.path, "kustomize:"), tree.path+string(filepath.Separator))
		if doc.namespace == nil {
			doc.namespace = &check.scope.Namespace
		}
		owned = append(owned, doc)
	}
	return owned, nil
}

func identityOwnedObject(doc manifest, service string) bool {
	_, workload := podSpec(doc)
	return workload || doc.group == rbacAPIGroup ||
		(doc.group == "" && (doc.kind == kindServiceAccount ||
			(doc.kind == kindConfigMap && metadataString(doc.value, keyName) == service+"-workload-peers")))
}

// Judge the final overlay, including authored patches targeting resources added
// by the projection. The expected data comes from the composition, never from a
// generated file that Kustomize can transform. This is part of the service
// configuration validation that must succeed before the tree can be promoted.
func validateProjectedWorkloadSVID(tree *renderRoot, root string, service *resources.Service, env *environments.Environment, scope unitScope) error {
	unique := resources.ServiceUnique(scope.Module, service.Name)
	if _, exists := scope.WorkloadPeers[unique]; !exists && !requestsWorkloadIdentity(service) {
		return nil
	}
	overlay, err := serviceRenderOverlay(tree, root, scope.Module, service.Name, env.Name)
	if err != nil {
		return err
	}
	documents, err := overlayManifestsInTree(tree, overlay)
	if err != nil {
		return err
	}
	return validateWorkloadSVIDManifests(documents, service, env, scope)
}

// Compare the complete output of each identity service's own overlay with the
// whole applied tree. Global identity uniqueness is checked before ownership;
// a sibling cannot replace any object even when its bytes happen to be equal.
func validateWorkloadIdentityChecks(documents []manifest, checks []workloadIdentityCheck) error {
	for i := range checks {
		check := &checks[i]
		own := check.objects
		if len(own) == 0 {
			return fmt.Errorf("service %s has no recorded render at %s", check.service.Name, check.path)
		}
		if !requestsWorkloadIdentity(check.service) {
			var unit []manifest
			for _, doc := range documents {
				if manifestFromService(doc, check.path) {
					unit = append(unit, doc)
				}
			}
			if err := validateWorkloadSVIDManifests(unit, check.service, check.environment, check.scope); err != nil {
				return err
			}
			continue
		}
		if err := validateWorkloadSVIDManifests(own, check.service, check.environment, check.scope); err != nil {
			return err
		}
		if err := validateOwnedObjects(documents, check); err != nil {
			return err
		}
	}
	return nil
}

func validateOwnedObjects(documents []manifest, check *workloadIdentityCheck) error {
	var conflicts []error
	expectedIdentities := make(map[objectIdentity]bool, len(check.objects))
	for _, expected := range check.objects {
		id := expected.identity(check.scope.Namespace)
		expectedIdentities[id] = true
		found := false
		for _, actual := range documents {
			if actual.identity("") != id {
				continue
			}
			found = true
			equal, err := sameObjectBytes(expected, actual)
			if err != nil {
				return err
			}
			if !equal || !manifestFromService(actual, check.path) {
				conflicts = append(conflicts, fmt.Errorf("%s at %s differs from its owning service render at %s", id, actual.path, expected.path))
			}
		}
		if !found {
			conflicts = append(conflicts, fmt.Errorf("%s from %s is missing from the promoted tree for %s/", id, expected.path, check.path))
		}
	}
	for _, actual := range documents {
		if manifestFromService(actual, check.path) && identityOwnedObject(actual, check.service.Name) && !expectedIdentities[actual.identity("")] {
			conflicts = append(conflicts, fmt.Errorf("%s at %s was not produced by its owning service render at %s/", actual.identity(""), actual.path, check.path))
		}
	}
	return errors.Join(conflicts...)
}

func manifestFromService(doc manifest, servicePath string) bool {
	return strings.HasPrefix(strings.TrimPrefix(doc.path, "kustomize:"), servicePath+"/")
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
	var found *manifest
	for i := range documents {
		doc := &documents[i]
		if doc.kind != kindConfigMap || metadataString(doc.value, keyName) != name {
			continue
		}
		if found != nil {
			return fmt.Errorf("ConfigMap %s/%s must occur exactly once in the effective workload peer plan: produced by both %s and %s", namespace, name, found.path, doc.path)
		}
		found = doc
	}
	if found == nil {
		return fmt.Errorf("ConfigMap %s/%s must occur exactly once in the effective workload peer plan", namespace, name)
	}

	expected := make(map[string]any, len(files))
	for key, value := range files {
		expected[key] = value
	}
	if found.group != "" || !reflect.DeepEqual(mapField(found.value, dataKey), expected) || len(mapField(found.value, "binaryData")) != 0 {
		return fmt.Errorf("ConfigMap %s/%s at %s differs from the composition workload peer plan", namespace, name, found.path)
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
