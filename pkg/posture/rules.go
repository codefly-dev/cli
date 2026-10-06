package posture

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// configMapKind names a ConfigMap reference, the one reference kind an effective
// manifest set can resolve for itself.
const configMapKind = "configMap"

// The manifest fields a pod specification is reached through.
const (
	specField        = "spec"
	templateField    = "template"
	jobTemplateField = "jobTemplate"
)

// scratchVolumeSource is what a scratch volume must be: writable space the platform
// put there at start, carrying nothing. A declaration cannot make a ConfigMap, a
// Secret or a claim into scratch.
const scratchVolumeSource = "emptyDir"

// durableVolumeSource is a claim-backed volume: the workload's own state, which the
// storage declaration governs.
const durableVolumeSource = "persistentVolumeClaim"

// podSpecPaths maps a workload kind to the field path of its pod specification. A
// kind absent here is still inspected when it carries a pod template at one of
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

// containerFields are the three container lists a pod specification may carry. All
// three run in the cell, so all three configure the workload and all three are read.
var containerFields = []string{"containers", "initContainers", "ephemeralContainers"}

// certificateSecretTypes are the Secret types whose content IS certificate material
// by Kubernetes' own definition.
var certificateSecretTypes = map[string]bool{"kubernetes.io/tls": true}

// certificateKeys are Kubernetes' conventional data keys for certificate material.
// They are a convention, not a guess about a file name: material under one of these
// is material whatever it contains, and material under any other key is recognised
// by PARSING its value, never by its extension (see certificateMaterialValue).
var certificateKeys = map[string]bool{
	"tls.crt": true, "tls.key": true, "ca.crt": true, "ca.key": true,
}

// pemBlocks are the PEM headers that make a value certificate or key material. This
// is the parse that replaced an extension list: `license.key` holding a licence is
// not material, and `ROOT_CA` holding a certificate is.
var pemBlocks = []string{
	"-----BEGIN CERTIFICATE-----", "-----BEGIN TRUSTED CERTIFICATE-----",
	"-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----",
	"-----BEGIN EC PRIVATE KEY-----", "-----BEGIN ENCRYPTED PRIVATE KEY-----",
	"-----BEGIN CERTIFICATE REQUEST-----", "-----BEGIN PKCS7-----",
}

// transportWords mark a setting that configures transport security, matched per
// word so an unrelated name that merely contains the letters is not one.
var transportWords = map[string]bool{
	"TLS": true, "MTLS": true, "SSL": true, "CERT": true, "CERTS": true,
	"CERTIFICATE": true, "CERTIFICATES": true, "CA": true, "CACERT": true,
	"TRUSTSTORE": true, "KEYSTORE": true, "PKI": true,
}

// listenerWords name the setting that says what a workload SERVES on. URL and
// ENDPOINT are deliberately absent: an external destination a workload dials is not
// a listener of its own, and reading one as TLS it terminates was a false refusal.
var listenerWords = map[string]bool{
	"LISTEN": true, "LISTENER": true, "BIND": true, "ADDR": true, "ADDRESS": true, "SERVE": true,
}

// developmentWords mark a development mode, matched per word so that a name merely
// containing the letters (…DEVICES) is not one.
var developmentWords = map[string]bool{"DEV": true, "DEVELOPMENT": true}

// stateWords and stateSelectorWords together name a setting that SELECTS where a
// workload keeps its state, rather than one that merely mentions storage.
var stateWords = map[string]bool{
	"STORAGE": true, "BACKEND": true, "STORE": true, "PERSISTENCE": true,
	"STATE": true, "DATABASE": true, "DB": true, "DATA": true,
}

var stateSelectorWords = map[string]bool{
	"MODE": true, "BACKEND": true, "ENGINE": true, "TYPE": true, "DRIVER": true,
	"PROVIDER": true, "KIND": true, "STORAGE": true, "PERSISTENCE": true,
}

// ephemeralStateValues are the values of a state setting that keep state only for
// the life of the process.
var ephemeralStateValues = map[string]bool{
	"memory": true, "inmem": true, "in-memory": true, "in_memory": true,
	"inmemory": true, "ram": true, "ephemeral": true, "tmpfs": true, valueNone: true,
}

// inMemoryMarkers name a setting that selects in-memory storage outright.
var inMemoryMarkers = []string{"inmem", "inmemory"}

// ValidateDocuments holds a selected manifest set to the deployed posture.
//
// Three invariants, in this order, because each depends on the one before:
//
//  1. MANDATORY — a deployed workload whose service declares no storage mode, or no
//     transport, is refused by name. Absence is never conformance: a contract that
//     admits what declared nothing enforces nothing.
//  2. CONSISTENT — the rendered configuration must agree with the declaration. A
//     declaration is the service's statement of intent; the manifest its agent
//     emitted beside it is evidence. Where they contradict, the render refuses and
//     names both sides. An unrecognised spelling cannot defeat this, because the
//     declaration still governs what the runtime may do.
//  3. FAIL CLOSED — a manifest this guard cannot read, a reference it cannot
//     resolve, and a selection it cannot complete are refusals, not successes.
func ValidateDocuments(documents []Document, defaults Subject, contracts Contracts, declaration *Declaration) error {
	index := newReferenceIndex(documents)
	for _, document := range documents {
		// Identity comes from selection, which read the real path; the caller's
		// defaults fill in what a path cannot say (the module a unit belongs to).
		subject := document.Subject
		if subject.Service == "" {
			subject.Service = defaults.Service
		}
		if subject.Module == "" {
			subject.Module = defaults.Module
		}
		if err := validateDocument(document, subject, index, contracts, declaration); err != nil {
			return err
		}
	}
	return declaration.validateContracts(contracts, documents)
}

// validateDocument applies every rule to one document.
func validateDocument(
	document Document,
	subject Subject,
	index *referenceIndex,
	contracts Contracts,
	declaration *Declaration,
) error {
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
	if kind == "" {
		return report(RuleNonScratchMount, "kind", "the manifest declares no kind, so this render cannot tell what it is; a manifest this guard cannot read is refused rather than assumed harmless")
	}
	if declaration.Asserted(AssertMeshProtectedTransport) {
		if err := checkDeliveredCertificates(document, report); err != nil {
			return err
		}
	}
	specs := podSpecifications(document.Value, kind)
	if len(specs) == 0 {
		return nil
	}
	declared, present := contracts.Of(subject)
	contract := &declared
	if err := checkDeclarationsArePresent(subject, present, contract, report); err != nil {
		return err
	}
	namespace := metadataNamespace(document.Value)
	for _, spec := range specs {
		if err := spec.malformed(report); err != nil {
			return err
		}
		if err := checkDeclaredVolumesOnly(document.Value, spec, subject, contract, report); err != nil {
			return err
		}
		if err := checkStorageAgreement(spec, subject, contract, index, namespace, report); err != nil {
			return err
		}
		if err := checkTransportAgreement(spec, subject, contract, index, namespace, declaration, report); err != nil {
			return err
		}
	}
	return nil
}

// reporter turns a rule, a field path and a detail into a violation, or into nil
// when the environment allows that rule for this subject.
type reporter func(rule, field, detail string) error

// checkDeclarationsArePresent is invariant 1. A deployed workload says what it does
// with its state and its transport, or it is not deployed. There is no success path
// through an absent declaration — the in-memory store this posture exists for has
// no claim to detect, so a rule that asked for a declaration only when it saw one
// never asked the service that needed asking.
func checkDeclarationsArePresent(subject Subject, present bool, contract *ServiceContract, report reporter) error {
	if !present || contract.StorageMode == "" {
		if err := report(RuleInMemoryStateStore, "spec.deployment."+storageKey, fmt.Sprintf(
			"service %s declares no storage mode, and a deployed workload says where its state lives before it is deployed: "+
				"declare spec.deployment.%s: %s (or %s, which a deployed render refuses)",
			subject, storageKey, StorageModeDurable, StorageModeEphemeral)); err != nil {
			return err
		}
	}
	if contract.StorageMode == StorageModeEphemeral {
		if err := report(RuleInMemoryStateStore, "spec.deployment."+storageKey, fmt.Sprintf(
			"service %s declares %s storage, and a deployed runtime keeps its state in a durable store: an ephemeral one loses, "+
				"on the next restart or node replacement, everything it held",
			subject, StorageModeEphemeral)); err != nil {
			return err
		}
	}
	if !present || contract.Transport == "" {
		if err := report(RulePeerTransportMaterial, "spec.deployment."+transportKey, fmt.Sprintf(
			"service %s declares no transport, and a deployed workload says who carries its transport before it is deployed: "+
				"declare spec.deployment.%s: %s (the platform carries it) or %s",
			subject, transportKey, TransportMesh, TransportOwnTLS)); err != nil {
			return err
		}
	}
	return nil
}

// checkStorageAgreement is invariant 2 for storage: a service that declared durable
// storage, beside rendered configuration that selects a development or in-memory
// mode, is a contradiction. The declaration is not evidence about the process; it is
// a statement the manifest must not contradict.
func checkStorageAgreement(
	spec pathedSpec,
	subject Subject,
	contract *ServiceContract,
	index *referenceIndex,
	namespace string,
	report reporter,
) error {
	if contract.StorageMode != StorageModeDurable {
		return nil
	}
	for _, configuration := range configurations(spec, index, namespace) {
		for position := range configuration.settings {
			entry := &configuration.settings[position]
			reason := ephemeralStateReason(entry)
			if reason == "" {
				continue
			}
			if err := report(RuleInMemoryStateStore, entry.field, fmt.Sprintf(
				"service %s declares %s storage, but container %q is configured with %s, which %s: "+
					"the declaration and the manifest contradict each other, and the render refuses rather than choose between them",
				subject, StorageModeDurable, configuration.container, entry.describe(), reason)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ephemeralStateReason reports why a setting selects state that does not survive the
// process, or "" when it does not.
func ephemeralStateReason(entry *setting) string {
	words := nameWords(entry.name)
	switch {
	case anyWord(words, developmentWords) && entry.enabled():
		return "starts its development server"
	case containsAny(canonical(entry.name), inMemoryMarkers) && entry.enabled():
		return "selects in-memory storage"
	case anyWord(words, stateWords) && anyWord(words, stateSelectorWords):
		if !entry.resolved {
			return "selects a storage mode this render cannot read"
		}
		if ephemeralStateValues[strings.ToLower(strings.TrimSpace(entry.value))] {
			return "keeps its state in memory"
		}
	}
	return ""
}

// checkTransportAgreement is invariant 2 for transport, and the mesh rule itself.
// A service that declared the platform carries its transport, beside rendered
// configuration that terminates TLS of its own, is a contradiction; a service that
// declared its own TLS is refused where the environment states the mesh already
// carries it.
func checkTransportAgreement(
	spec pathedSpec,
	subject Subject,
	contract *ServiceContract,
	index *referenceIndex,
	namespace string,
	declaration *Declaration,
	report reporter,
) error {
	if contract.Transport == TransportOwnTLS {
		if !declaration.Asserted(AssertMeshProtectedTransport) {
			return nil
		}
		return report(RulePeerTransportMaterial, "spec.deployment."+transportKey, fmt.Sprintf(
			"service %s declares it terminates its own TLS, but the environment asserts %s: transport between workloads belongs "+
				"to the mesh and TLS at the edge belongs to the ingress",
			subject, AssertMeshProtectedTransport))
	}
	if contract.Transport != TransportMesh {
		return nil
	}
	for _, configuration := range configurations(spec, index, namespace) {
		for position := range configuration.settings {
			entry := &configuration.settings[position]
			reason := transportReason(entry)
			if reason == "" {
				continue
			}
			if err := report(RulePeerTransportMaterial, entry.field, fmt.Sprintf(
				"service %s declares the platform carries its transport (%s: %s), but container %q is configured with %s, which %s: "+
					"the declaration and the manifest contradict each other, and the render refuses rather than choose between them",
				subject, transportKey, TransportMesh, configuration.container, entry.describe(), reason)); err != nil {
				return err
			}
		}
	}
	return nil
}

// transportReason reports why a setting configures transport the workload terminates
// itself, or "" when it does not.
func transportReason(entry *setting) string {
	words := nameWords(entry.name)
	if anyWord(words, listenerWords) &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.value)), "https://") {
		return "serves TLS itself"
	}
	if certificateMaterialValue(entry.value) {
		return "carries certificate material inline"
	}
	if !anyWord(words, transportWords) {
		return ""
	}
	if !entry.resolved {
		return "names transport material this render cannot read"
	}
	if entry.enabled() {
		return "configures the workload's own TLS"
	}
	return ""
}

// checkDeclaredVolumesOnly admits exactly what the service declared: a scratch
// volume it declared, mounted at the one path declared for it, or its own durable
// state when it declared durable storage.
//
// A declared NAME is not enough: the volume must actually be scratch. A claim
// template named like the scratch volume is a claim, and resolving the name to its
// real source is what keeps a matching name and destination from establishing one.
func checkDeclaredVolumesOnly(
	document map[string]any,
	spec pathedSpec,
	subject Subject,
	contract *ServiceContract,
	report reporter,
) error {
	durable := contract.StorageMode == StorageModeDurable
	sources := effectiveVolumeSources(document, spec)
	for _, volume := range volumes(spec) {
		_, isScratch := contract.scratchVolume(volume.name)
		switch {
		case isScratch && volume.source == scratchVolumeSource:
			continue
		case isScratch:
			if err := report(RuleNonScratchMount, volume.sourcePath, fmt.Sprintf(
				"volume %q is declared as scratch by service %s but is rendered from a %s source; scratch space is an %s and carries "+
					"nothing the platform did not put there at start",
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
		source := sources[mount.volume]
		declaredScratch, isScratch := contract.scratchVolume(mount.volume)
		switch {
		case isScratch && source == scratchVolumeSource && declaredScratch.Mount == mount.path:
			continue
		case isScratch:
			// A declared scratch name whose volume is not scratch, or is mounted
			// somewhere else, is refused below: the declaration is about a scratch
			// volume, and a claim that borrows its name is not one.
		case source == durableVolumeSource && durable:
			continue
		}
		detail := fmt.Sprintf(
			"volume %q is mounted at %s and service %s declares no such scratch volume there (declared: %s); a deployed workload "+
				"mounts only the scratch volumes it declares and its own durable state",
			mount.volume, mount.path, subject, contract.declaredScratch())
		if isScratch && source != scratchVolumeSource {
			detail = fmt.Sprintf(
				"volume %q is mounted at %s as service %s's declared scratch volume, but it is backed by a %s source, not an %s: "+
					"a name and a destination do not make a volume scratch",
				mount.volume, mount.path, subject, source, scratchVolumeSource)
		} else if isScratch {
			detail = fmt.Sprintf(
				"volume %q is mounted at %s, but service %s declares it at %s: a scratch volume is a name AND the one path it is mounted at",
				mount.volume, mount.path, subject, declaredScratch.Mount)
		}
		if err := report(RuleNonScratchMount, mount.field, detail); err != nil {
			return err
		}
	}
	if durable {
		if err := checkClaimIsNotConfiguration(spec, subject, sources, report); err != nil {
			return err
		}
	}
	for _, device := range volumeDevices(spec) {
		if sources[device.volume] == durableVolumeSource && durable {
			continue
		}
		if err := report(RuleNonScratchMount, device.field, fmt.Sprintf(
			"volume %q is attached as a raw device at %s, which service %s has not declared; a block device is another way to reach "+
				"storage the platform did not deliver",
			device.volume, device.path, subject)); err != nil {
			return err
		}
	}
	return nil
}

// configurationWords name a setting that tells a container where to read its
// configuration from.
var configurationWords = map[string]bool{
	"CONFIG": true, "CONFIGURATION": true, "CONF": true, "SETTINGS": true, "CONFIGFILE": true,
}

// checkClaimIsNotConfiguration refuses a durable claim that is delivering
// configuration rather than holding the workload's own state. A durable declaration
// says "this is my state"; a container configured to READ its configuration from a
// path inside that claim says otherwise, and the two cannot both be true.
//
// It is found by comparing the claim's mount path with the paths the container's own
// configuration names — a parse of both sides, not a list of directories someone
// could rename around.
func checkClaimIsNotConfiguration(
	spec pathedSpec,
	subject Subject,
	sources map[string]string,
	report reporter,
) error {
	claims := map[string]string{}
	for _, mount := range mounts(spec) {
		if sources[mount.volume] == durableVolumeSource && mount.path != "" {
			claims[mount.volume] = mount.path
		}
	}
	if len(claims) == 0 {
		return nil
	}
	for _, container := range containers(spec) {
		settings := container.argumentSettings()
		settings = append(settings, container.environmentSettings(&referenceIndex{}, "")...)
		for position := range settings {
			entry := &settings[position]
			if !anyWord(nameWords(entry.name), configurationWords) || !entry.resolved || entry.value == "" {
				continue
			}
			for _, volume := range sortedStrings(mapKeys(claims)) {
				mountPath := claims[volume]
				if !underPath(entry.value, mountPath) {
					continue
				}
				if err := report(RuleNonScratchMount, entry.field, fmt.Sprintf(
					"service %s declares %s storage, and volume %q is admitted as the workload's own state at %s — but %s reads this "+
						"container's configuration from inside it: a durable claim that delivers configuration is not state, and "+
						"configuration reaches a service as values and secrets in the environment variables the render projects",
					subject, StorageModeDurable, volume, mountPath, entry.describe())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// underPath reports whether a value is a filesystem path at or below a mount point.
func underPath(value, mountPath string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "/") {
		return false
	}
	clean := strings.TrimRight(mountPath, "/")
	return value == clean || strings.HasPrefix(value, clean+"/")
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// effectiveVolumeSources resolves every volume name a container can mount to the
// source actually behind it: a pod volume's own source, or persistentVolumeClaim for
// a StatefulSet claim template, which is a volume no volumes list mentions.
func effectiveVolumeSources(document map[string]any, spec pathedSpec) map[string]string {
	sources := map[string]string{}
	if workloadSpec, ok := document[specField].(map[string]any); ok {
		claims, _ := workloadSpec["volumeClaimTemplates"].([]any)
		for _, entry := range claims {
			claim, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if metadata, present := claim["metadata"].(map[string]any); present {
				if name := stringAt(metadata, "name"); name != "" {
					sources[name] = durableVolumeSource
				}
			}
		}
	}
	for _, volume := range volumes(spec) {
		sources[volume.name] = volume.source
	}
	return sources
}

// checkDeliveredCertificates refuses certificate material the render itself
// delivers, where the environment has said transport between workloads is already
// protected. Material is established by Kubernetes' own typing and conventional
// keys, or by PARSING the value — never by a file extension, which cannot tell an
// application's licence key from a private key.
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
			for _, key := range sortedMapKeys(values) {
				text, _ := values[key].(string)
				conventional := certificateKeys[strings.ToLower(key)]
				if !conventional && !certificateMaterialValue(text) {
					continue
				}
				because := "its value is certificate material"
				if conventional {
					because = "it is one of Kubernetes' certificate keys"
				}
				if err := report(RulePeerTransportMaterial, field+"."+key, fmt.Sprintf(
					"the render delivers certificate material under %s.%s (%s); %s", field, key, because, detail)); err != nil {
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

// certificateMaterialValue reports whether a value IS certificate or key material,
// by parsing it for a PEM block.
func certificateMaterialValue(value string) bool {
	for _, block := range pemBlocks {
		if strings.Contains(value, block) {
			return true
		}
	}
	return false
}

// certificateVolumeKey reports the material a volume source delivers: one of
// Kubernetes' conventional certificate keys named by an item.
func certificateVolumeKey(source any) (string, bool) {
	switch typed := source.(type) {
	case map[string]any:
		for _, field := range []string{"key", "path"} {
			if text, ok := typed[field].(string); ok && certificateKeys[strings.ToLower(lastPathSegment(text))] {
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

// validateContracts holds the declarations themselves to the posture: an in-cell
// endpoint a service declares with a TLS protocol is refused where the environment
// asserts the mesh.
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
				Field: fmt.Sprintf("endpoints[%s].api = %s", endpoint.Name, endpoint.Protocol),
				Detail: fmt.Sprintf(
					"service %s declares in-cell endpoint %q with the %s protocol, which is transport it terminates itself, "+
						"but the environment asserts %s: transport between workloads belongs to the mesh and TLS at the edge to the ingress",
					subject, endpoint.Name, endpoint.Protocol, AssertMeshProtectedTransport),
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

// malformed refuses a pod specification this guard cannot read: a collection where a
// list belongs, or an entry that is not an object. A shape the guard skips is a
// shape it has not checked, and an unchecked shape must not read as conforming.
func (spec pathedSpec) malformed(report reporter) error {
	at := strings.Join(spec.path, ".")
	for _, field := range append([]string{"volumes"}, containerFields...) {
		value, present := spec.spec[field]
		if !present {
			continue
		}
		list, ok := value.([]any)
		if !ok {
			return report(RuleNonScratchMount, at+"."+field, fmt.Sprintf(
				"%s.%s is not a list, so this render cannot read what the workload carries; a manifest this guard cannot read is refused",
				at, field))
		}
		for index, entry := range list {
			if _, ok := entry.(map[string]any); !ok {
				return report(RuleNonScratchMount, fmt.Sprintf("%s.%s[%d]", at, field, index), fmt.Sprintf(
					"%s.%s[%d] is not an object, so this render cannot read it; a manifest this guard cannot read is refused",
					at, field, index))
			}
		}
	}
	return nil
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

// volumes decodes a pod specification's volumes. A volume with more than one source
// is reported with the special source "several", which no rule admits: Kubernetes
// allows exactly one, and inspecting only the first would check a volume the cell
// does not have.
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
		var sources []string
		for _, key := range sortedMapKeys(value) {
			if key == "name" {
				continue
			}
			sources = append(sources, key)
		}
		switch len(sources) {
		case 0:
		case 1:
			volume.source = sources[0]
			volume.sourcePath = fmt.Sprintf("%s.volumes[%d].%s", strings.Join(spec.path, "."), index, sources[0])
		default:
			volume.source = "several (" + strings.Join(sources, ", ") + ")"
			volume.sourcePath = fmt.Sprintf("%s.volumes[%d]", strings.Join(spec.path, "."), index)
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
	return attachments(spec, "volumeMounts", "mountPath")
}

// volumeDevices decodes every raw block device attachment: another way to reach
// storage, and one a mount-only check does not see.
func volumeDevices(spec pathedSpec) []pathedMount {
	return attachments(spec, "volumeDevices", "devicePath")
}

func attachments(spec pathedSpec, field, pathKey string) []pathedMount {
	var decoded []pathedMount
	for _, container := range containers(spec) {
		list, ok := container.value[field].([]any)
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
				path:   stringAt(value, pathKey),
				field:  fmt.Sprintf("%s.%s[%d]", container.path, field, index),
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

// environmentEntry is one env entry of a container.
type environmentEntry struct {
	name       string
	value      string
	hasLiteral bool
	reference  *valueReference
}

func (container pathedContainer) environment() []environmentEntry {
	list, ok := container.value["env"].([]any)
	if !ok {
		return nil
	}
	entries := make([]environmentEntry, 0, len(list))
	for _, entry := range list {
		value, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		decoded := environmentEntry{name: stringAt(value, "name")}
		if literal, exists := value["value"]; exists {
			decoded.value, decoded.hasLiteral = fmt.Sprintf("%v", literal), true
		}
		if from, exists := value["valueFrom"].(map[string]any); exists {
			decoded.reference = decodeValueReference(from)
		}
		entries = append(entries, decoded)
	}
	return entries
}

// decodeValueReference names where a non-literal env value comes from.
func decodeValueReference(from map[string]any) *valueReference {
	for field, kind := range map[string]string{
		"configMapKeyRef": configMapKind, "secretKeyRef": "secret",
		"fieldRef": "field", "resourceFieldRef": "resource",
	} {
		reference, ok := from[field].(map[string]any)
		if !ok {
			continue
		}
		name := stringAt(reference, "name")
		switch kind {
		case "field":
			name = stringAt(reference, "fieldPath")
		case "resource":
			name = stringAt(reference, "resource")
		}
		return &valueReference{kind: kind, name: name, key: stringAt(reference, "key")}
	}
	return nil
}

// argumentToken is one token of a container's command or args.
type argumentToken struct {
	value string
	path  string
}

// arguments decodes a container's command and args as individual tokens.
func (container pathedContainer) arguments() []argumentToken {
	var tokens []argumentToken
	for _, field := range []string{"command", "args"} {
		list, ok := container.value[field].([]any)
		if !ok {
			continue
		}
		for index, entry := range list {
			text, ok := entry.(string)
			if !ok {
				continue
			}
			tokens = append(tokens, argumentToken{
				value: text,
				path:  fmt.Sprintf("%s.%s[%d]", container.path, field, index),
			})
		}
	}
	return tokens
}

// canonical is the separator-free lowercase spelling a marker is matched against.
func canonical(value string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(value) {
		if unicode.IsLetter(symbol) || unicode.IsDigit(symbol) {
			builder.WriteRune(symbol)
		}
	}
	return builder.String()
}

// nameWords splits a configuration name into upper-case words, on separators and on
// camelCase boundaries, so a marker matches a word and not a substring.
func nameWords(value string) map[string]bool {
	words := map[string]bool{}
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words[strings.ToUpper(current.String())] = true
			current.Reset()
		}
	}
	runes := []rune(value)
	for index, symbol := range runes {
		switch {
		case unicode.IsLetter(symbol) || unicode.IsDigit(symbol):
			if unicode.IsUpper(symbol) && index > 0 && unicode.IsLower(runes[index-1]) {
				flush()
			}
			current.WriteRune(symbol)
		default:
			flush()
		}
	}
	flush()
	return words
}

func anyWord(words, markers map[string]bool) bool {
	for word := range words {
		if markers[word] {
			return true
		}
	}
	return false
}

func containsAny(canonical string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(canonical, marker) {
			return true
		}
	}
	return false
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
