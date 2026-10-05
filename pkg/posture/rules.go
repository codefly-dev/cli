package posture

import (
	"fmt"
	"sort"
	"strings"
)

// The manifest fields a pod specification is reached through.
const (
	specField        = "spec"
	templateField    = "template"
	jobTemplateField = "jobTemplate"
)

// scratchVolumeSource is the Kubernetes source a scratch volume must be: writable
// space the platform put there at start, carrying nothing. A declaration cannot
// turn a ConfigMap into scratch.
const scratchVolumeSource = "emptyDir"

// durableVolumeSource is a claim-backed volume: the workload's own state, which
// the storage declaration governs (checkStorageDeclaration), not this rule.
const durableVolumeSource = "persistentVolumeClaim"

// podSpecPaths maps a workload kind to the field path of its pod specification.
// A kind absent here is still inspected when it carries a pod template at one of
// these paths, so a workload CRD is covered without being enumerated.
var podSpecPaths = map[string][]string{
	"Pod":                   {specField},
	"Deployment":            templatePath,
	"StatefulSet":           templatePath,
	"DaemonSet":             templatePath,
	"ReplicaSet":            templatePath,
	"ReplicationController": templatePath,
	"Job":                   templatePath,
	"CronJob":               jobTemplatePath,
}

var (
	templatePath    = []string{specField, templateField, specField}
	jobTemplatePath = []string{specField, jobTemplateField, specField, templateField, specField}
)

var genericPodSpecPaths = [][]string{templatePath, jobTemplatePath}

// containerFields are the three container lists a pod specification may carry.
// All three run in the cell, so all three are held to the same rules.
var containerFields = []string{"containers", "initContainers", "ephemeralContainers"}

// persistentStateSources are the volume sources that keep a workload's own state
// across restarts. A workload that carries one is a store, whatever it is called:
// this is the one thing about storage a manifest really does say, and it is what
// makes a missing storage declaration a refusal rather than a guess.
var persistentStateSources = map[string]bool{
	"persistentVolumeClaim": true, "ephemeral": true,
}

// certificateSecretTypes are the Secret types whose content IS certificate
// material, by Kubernetes' own definition.
var certificateSecretTypes = map[string]bool{
	"kubernetes.io/tls": true, "kubernetes.io/ssh-auth": false,
}

// certificateKeys are the data keys certificate material is delivered under. They
// are Kubernetes' own conventional names plus the file types material is encoded
// in, so this reads the SHAPE of what the render delivers rather than guessing
// from a name someone chose.
var certificateKeys = map[string]bool{
	"tls.crt": true, "tls.key": true, "ca.crt": true, "ca.key": true,
	"client.crt": true, "client.key": true, "server.crt": true, "server.key": true,
}

// certificateExtensions are the file types certificate and key material is
// delivered as; a data key or projected path ending in one of them is material.
var certificateExtensions = []string{".crt", ".cer", ".pem", ".key", ".p12", ".pfx", ".jks", ".der"}

// ValidateDocuments holds a selected manifest set to the deployed posture, with
// the contracts the services of that render declared about themselves.
//
// Every rule here is decided by one of two things only: what a service declared,
// or the structure of the manifests the cell would apply. Nothing is inferred
// from a container's command line or its environment variables — a render cannot
// prove from those what a process does, and the attempt both missed ordinary
// spellings and refused correct configurations.
func ValidateDocuments(documents []Document, defaults Subject, contracts Contracts, declaration *Declaration) error {
	for _, document := range documents {
		subject := SubjectFromPath(document.Location, defaults)
		if err := validateDocument(document, subject, contracts, declaration); err != nil {
			return err
		}
	}
	return declaration.validateContracts(contracts, documents)
}

// validateDocument applies the structural rules to one document.
func validateDocument(document Document, subject Subject, contracts Contracts, declaration *Declaration) error {
	kind := stringAt(document.Value, "kind")
	name := metadataName(document.Value)
	workload := kind
	if name != "" {
		workload = kind + "/" + name
	}
	report := func(rule, field, detail string) error {
		if _, allowed := declaration.Allows(rule, subject); allowed {
			return nil
		}
		return &Violation{
			Rule: rule, Subject: subject, Location: document.Location,
			Workload: workload, Field: field, Detail: detail,
		}
	}
	if declaration.Asserted(AssertMeshProtectedTransport) {
		if err := checkDeliveredCertificates(document, report); err != nil {
			return err
		}
	}
	declaredContract, _ := contracts.Of(subject)
	contract := &declaredContract
	for _, spec := range podSpecifications(document.Value, kind) {
		if err := checkDeclaredVolumesOnly(document.Value, spec, subject, contract, report); err != nil {
			return err
		}
		if err := checkStorageDeclaration(document.Value, spec, subject, contracts, report); err != nil {
			return err
		}
	}
	return nil
}

// reporter turns a rule, a field path and a detail into a violation, or into nil
// when the environment allows that rule for this subject. A caller keeps looking
// after a nil: an allowance covers one rule for one service.
type reporter func(rule, field, detail string) error

// checkDeclaredVolumesOnly admits exactly what the service declared and nothing
// else: a scratch volume it declared (spec.deployment.scratch-volumes), mounted at
// the one path it declared for it, or the workload's own durable state when it
// declared durable storage. Every other volume and every other mount is refused by
// name.
//
// The admitted set is a declaration rather than a list of sources, names or
// destinations, because no property of a volume separates the platform's scratch
// directory from a second emptyDir mounted over /secrets to deliver credentials —
// both are an emptyDir — and a rule keyed on the destination is defeated by moving
// the mount. What the platform renders, the platform declares; what nobody
// declared does not reach a cell.
func checkDeclaredVolumesOnly(
	document map[string]any,
	spec pathedSpec,
	subject Subject,
	contract *ServiceContract,
	report reporter,
) error {
	durable := contract.StorageMode == StorageModeDurable
	state := claimNames(document, spec)
	for _, volume := range volumes(spec) {
		_, isScratch := contract.scratchVolume(volume.name)
		switch {
		case isScratch && volume.source == scratchVolumeSource:
			continue
		case isScratch:
			if err := report(RuleNonScratchMount, volume.sourcePath, fmt.Sprintf(
				"volume %q is declared as scratch by service %s but is rendered from a %s source; scratch space is an %s and carries nothing the platform did not put there at start",
				volume.name, subject, volume.source, scratchVolumeSource)); err != nil {
				return err
			}
			continue
		case volume.source == durableVolumeSource && durable:
			continue
		}
		detail := fmt.Sprintf(
			"volume %q (%s) is not declared by service %s, and a deployed workload carries only what its service declares: "+
				"scratch space in spec.deployment.%s (declared: %s)",
			volume.name, volume.source, subject, scratchVolumesKey, contract.declaredScratch())
		if volume.source == durableVolumeSource {
			detail = fmt.Sprintf(
				"volume %q is the workload's own durable state, and service %s has not declared spec.deployment.%s: %s",
				volume.name, subject, storageKey, StorageModeDurable)
		}
		if err := report(RuleNonScratchMount, volume.sourcePath, detail); err != nil {
			return err
		}
	}
	for _, mount := range mounts(spec) {
		if declared, isScratch := contract.scratchVolume(mount.volume); isScratch {
			if declared.Mount == mount.path {
				continue
			}
			if err := report(RuleNonScratchMount, mount.field, fmt.Sprintf(
				"volume %q is mounted at %s, but service %s declares it at %s: a scratch volume is a name AND the one path it is mounted at, "+
					"or the same volume mounted twice is another way to put files in front of a process",
				mount.volume, mount.path, subject, declared.Mount)); err != nil {
				return err
			}
			continue
		}
		if state[mount.volume] && durable {
			continue
		}
		if err := report(RuleNonScratchMount, mount.field, fmt.Sprintf(
			"volume %q is mounted at %s and service %s declares no such volume; a deployed workload mounts only the scratch volumes it declares "+
				"(declared: %s) and its own durable state",
			mount.volume, mount.path, subject, contract.declaredScratch())); err != nil {
			return err
		}
	}
	return nil
}

// claimNames are the volume names that carry the workload's own durable state: a
// StatefulSet's claim templates, and any claim-backed volume.
func claimNames(document map[string]any, spec pathedSpec) map[string]bool {
	names := map[string]bool{}
	if workloadSpec, ok := document[specField].(map[string]any); ok {
		claims, _ := workloadSpec["volumeClaimTemplates"].([]any)
		for _, entry := range claims {
			claim, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if metadata, present := claim["metadata"].(map[string]any); present {
				if name := stringAt(metadata, "name"); name != "" {
					names[name] = true
				}
			}
		}
	}
	for _, volume := range volumes(spec) {
		if volume.source == durableVolumeSource {
			names[volume.name] = true
		}
	}
	return names
}

// checkStorageDeclaration holds a workload that keeps its own state to what its
// service declared about that state. A workload carrying persistent storage is a
// store — that is what the volume means — so its service must declare the storage
// durable; declaring it ephemeral, or declaring nothing at all, is refused by
// name. The render does not try to decide from a command line which mode a
// process will choose: it refuses to deploy a store that has not said.
func checkStorageDeclaration(document map[string]any, spec pathedSpec, subject Subject, contracts Contracts, report reporter) error {
	state := persistentState(document, spec)
	contract, declared := contracts.Of(subject)
	if declared && contract.StorageMode == StorageModeEphemeral {
		return report(RuleInMemoryStateStore, "spec.deployment."+storageKey, fmt.Sprintf(
			"service %s declares %s storage, and a deployed runtime keeps its state in a durable store: "+
				"an ephemeral one loses, on the next restart or node replacement, everything it held",
			subject, StorageModeEphemeral))
	}
	if state == "" {
		return nil
	}
	if !declared || contract.StorageMode == "" {
		return report(RuleInMemoryStateStore, state, fmt.Sprintf(
			"the workload keeps its own state (%s) but service %s declares no storage mode, so this render cannot show the state is durable; "+
				"declare spec.deployment.%s: %s (or %s)",
			state, subject, storageKey, StorageModeDurable, StorageModeEphemeral))
	}
	return nil
}

// persistentState reports the field by which a workload keeps state across
// restarts, or "" when it keeps none. A StatefulSet declares its claims beside
// its pod template rather than inside it, so both places are read.
func persistentState(document map[string]any, spec pathedSpec) string {
	if workloadSpec, ok := document[specField].(map[string]any); ok {
		if claims, present := workloadSpec["volumeClaimTemplates"].([]any); present && len(claims) > 0 {
			return specField + ".volumeClaimTemplates"
		}
	}
	for _, volume := range volumes(spec) {
		if persistentStateSources[volume.source] {
			return volume.sourcePath
		}
	}
	return ""
}

// checkDeliveredCertificates refuses certificate material the render itself
// delivers, on an environment that has already said transport between its
// workloads is protected. It reads what the manifests ARE — a Secret of a
// certificate type, a data key that is certificate material, a projected source
// carrying one — never what a container might do with them.
func checkDeliveredCertificates(document Document, report reporter) error {
	detail := "the environment asserts " + AssertMeshProtectedTransport +
		": transport between workloads belongs to the mesh and TLS at the edge belongs to the ingress, " +
		"so a deployed render delivers no certificate material of its own"
	kind := stringAt(document.Value, "kind")
	if kind == "Secret" && certificateSecretTypes[stringAt(document.Value, "type")] {
		if err := report(RulePeerTransportMaterial, "type", fmt.Sprintf(
			"the render delivers a %s Secret (%s)", stringAt(document.Value, "type"), detail)); err != nil {
			return err
		}
	}
	if kind == "Secret" || kind == "ConfigMap" {
		for _, field := range []string{"data", "stringData", "binaryData"} {
			values, _ := document.Value[field].(map[string]any)
			for _, key := range sortedKeysOf(values) {
				if !certificateMaterialKey(key) {
					continue
				}
				if err := report(RulePeerTransportMaterial, field+"."+key, fmt.Sprintf(
					"the render delivers certificate material under %s.%s (%s)", field, key, detail)); err != nil {
					return err
				}
			}
		}
	}
	for _, spec := range podSpecifications(document.Value, kind) {
		for _, volume := range volumes(spec) {
			if key, found := certificateVolumeKey(volume.value[volume.source]); found {
				if err := report(RulePeerTransportMaterial, volume.sourcePath, fmt.Sprintf(
					"volume %q delivers certificate material (%s) as files (%s)", volume.name, key, detail)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// certificateMaterialKey reports whether a data key names certificate or key
// material: one of Kubernetes' conventional names, or a file of a material type.
func certificateMaterialKey(key string) bool {
	lowered := strings.ToLower(key)
	if certificateKeys[lowered] {
		return true
	}
	for _, extension := range certificateExtensions {
		if strings.HasSuffix(lowered, extension) {
			return true
		}
	}
	return false
}

// certificateVolumeKey reports the material a volume source delivers: an item
// key or path that is certificate material. A volume naming a Secret this render
// does not carry says nothing about its content, so only material the manifests
// themselves name is read here.
func certificateVolumeKey(source any) (string, bool) {
	switch typed := source.(type) {
	case map[string]any:
		for _, field := range []string{"key", "path"} {
			if text, ok := typed[field].(string); ok && certificateMaterialKey(text) {
				return text, true
			}
		}
		for _, key := range sortedMapKeys(typed) {
			if material, found := certificateVolumeKey(typed[key]); found {
				return material, true
			}
		}
	case []any:
		for _, item := range typed {
			if material, found := certificateVolumeKey(item); found {
				return material, true
			}
		}
	}
	return "", false
}

// validateContracts holds the service contracts themselves to the posture: an
// endpoint a service declares as its own TLS is refused on a mesh-protected
// environment, from the declaration rather than from anything a container does.
func (declaration *Declaration) validateContracts(contracts Contracts, documents []Document) error {
	if !declaration.Asserted(AssertMeshProtectedTransport) || len(documents) == 0 {
		return nil
	}
	for _, key := range contracts.sortedSubjects() {
		declared := contracts[key]
		contract := &declared
		subject := Subject{Module: contract.Module, Service: contract.Service}
		if _, allowed := declaration.Allows(RulePeerTransportMaterial, subject); allowed {
			continue
		}
		for _, endpoint := range contract.declaredTLSEndpoints() {
			return &Violation{
				Rule: RulePeerTransportMaterial, Subject: subject,
				Field: fmt.Sprintf("endpoints[%s].api = %s", endpoint.Name, endpoint.API),
				Detail: fmt.Sprintf(
					"service %s declares endpoint %q with the %s protocol, which is transport it terminates itself, "+
						"but the environment asserts %s: transport between workloads belongs to the mesh and TLS at the edge to the ingress",
					subject, endpoint.Name, endpoint.API, AssertMeshProtectedTransport),
			}
		}
	}
	return nil
}

// pathedSpec is a pod specification and the field path it was found at.
type pathedSpec struct {
	path []string
	spec map[string]any
}

// podSpecifications locates every pod specification a document carries.
func podSpecifications(document map[string]any, kind string) []pathedSpec {
	paths, declared := podSpecPaths[kind]
	candidates := [][]string{paths}
	if !declared {
		candidates = genericPodSpecPaths
	}
	var specs []pathedSpec
	for _, path := range candidates {
		spec, ok := mapAt(document, path...)
		if !ok {
			continue
		}
		if !declared && spec["containers"] == nil {
			continue
		}
		specs = append(specs, pathedSpec{path: path, spec: spec})
	}
	return specs
}

// pathedVolume is one entry of a pod specification's volumes list.
type pathedVolume struct {
	name       string
	source     string
	sourcePath string
	value      map[string]any
}

// volumes decodes a pod specification's volumes, naming each one's single source.
func volumes(spec pathedSpec) []pathedVolume {
	list, ok := spec.spec["volumes"].([]any)
	if !ok {
		return nil
	}
	decoded := make([]pathedVolume, 0, len(list))
	for index, entry := range list {
		value, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		volume := pathedVolume{name: stringAt(value, "name"), value: value}
		for _, key := range sortedMapKeys(value) {
			if key == "name" {
				continue
			}
			volume.source = key
			volume.sourcePath = fmt.Sprintf("%s.volumes[%d].%s", strings.Join(spec.path, "."), index, key)
			break
		}
		decoded = append(decoded, volume)
	}
	return decoded
}

// pathedMount is one volumeMount of one container.
type pathedMount struct {
	volume string
	path   string
	field  string
}

// mounts decodes every volumeMount of every container of a pod specification.
func mounts(spec pathedSpec) []pathedMount {
	var decoded []pathedMount
	for _, container := range containers(spec) {
		list, ok := container.value["volumeMounts"].([]any)
		if !ok {
			continue
		}
		for index, entry := range list {
			value, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			decoded = append(decoded, pathedMount{
				volume: stringAt(value, "name"),
				path:   stringAt(value, "mountPath"),
				field:  fmt.Sprintf("%s.volumeMounts[%d]", container.path, index),
			})
		}
	}
	return decoded
}

// pathedContainer is one container of a pod specification.
type pathedContainer struct {
	path  string
	value map[string]any
}

// containers decodes every container of a pod specification.
func containers(spec pathedSpec) []pathedContainer {
	var decoded []pathedContainer
	for _, field := range containerFields {
		list, ok := spec.spec[field].([]any)
		if !ok {
			continue
		}
		for index, entry := range list {
			value, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			decoded = append(decoded, pathedContainer{
				path:  fmt.Sprintf("%s.%s[%d]", strings.Join(spec.path, "."), field, index),
				value: value,
			})
		}
	}
	return decoded
}

func stringAt(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func metadataName(document map[string]any) string {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	return stringAt(metadata, "name")
}

func mapAt(document map[string]any, path ...string) (map[string]any, bool) {
	current := document
	for _, segment := range path {
		next, ok := current[segment].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysOf(value map[string]any) []string { return sortedMapKeys(value) }
