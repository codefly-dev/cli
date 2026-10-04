package posture

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// scratchVolumeSource is the standard scratch volume: the one source a deployed
// workload may mount. It is writable space for a read-only root filesystem, and
// it carries nothing the platform did not put there at start.
const scratchVolumeSource = "emptyDir"

// durableVolumeSource is a durable data claim. It is not configuration delivery
// — it is the storage a stateful workload keeps its own data in, which
// RuleInMemoryStateStore exists to require — so it is admitted beside scratch.
// A StatefulSet's volumeClaimTemplates are not pod volumes at all and are never
// inspected here.
const durableVolumeSource = "persistentVolumeClaim"

// The manifest fields a pod specification is reached through.
const (
	specField        = "spec"
	templateField    = "template"
	jobTemplateField = "jobTemplate"
)

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

// templatePath and jobTemplatePath are the two pod-template locations in the
// Kubernetes workload API.
var (
	templatePath    = []string{specField, templateField, specField}
	jobTemplatePath = []string{specField, jobTemplateField, specField, templateField, specField}
)

// genericPodSpecPaths are the pod-specification locations tried for a kind
// podSpecPaths does not name.
var genericPodSpecPaths = [][]string{templatePath, jobTemplatePath}

// containerFields are the three container lists a pod specification may carry.
// All three run in the cell, so all three are held to the same rules.
var containerFields = []string{"containers", "initContainers", "ephemeralContainers"}

// transportMaterialMarkers name certificate, key and CA material in a field
// name, a flag or a value, matched against the separator-free lowercase
// spelling. Every marker naming a key also carries the transport word that makes
// it one (tls, ssl, client, server, peer, certificate): a bare "privatekey" is
// deliberately absent, because a signing key an application authenticates with —
// an app integration's private key — is a credential, not its transport, and a
// bare "keyfile" is absent for the same reason. Nothing is lost by it: a listener
// handed a key file names the file (tls.key, key.pem), and the certificate half
// of the pair ("certfile") is always present when one is configured.
var transportMaterialMarkers = []string{
	"cabundle", "cachain", "cacert", "certificatefile", "certificatekey", "certfile",
	"clientca", "clientcert", "clientkey", "mutualtls", "peercert", "peerkey",
	"servercert", "serverkey", "sslca", "sslcert", "sslkey",
	"truststore", "trustedca", "tlsca", "tlscert", "tlskey",
}

// transportMaterialFiles are the conventional file names TLS material is
// delivered under, matched inside a value or a volume source.
var transportMaterialFiles = []string{"tls.crt", "tls.key", "ca.crt", "ca.pem", "cert.pem", "key.pem"}

// transportMaterialNameWords are the words that name TLS material in a volume,
// a Secret or a ConfigMap — "store-peer-tls", "api-certs", "internal-ca". They
// are matched per word, and only inside a volume source: a word this short would
// read as credential material in far too many ordinary configuration names.
var transportMaterialNameWords = map[string]bool{
	"TLS": true, "MTLS": true, "SSL": true, "CA": true, "PKI": true,
	"CERT": true, "CERTS": true, "CERTIFICATE": true, "CERTIFICATES": true,
}

// developmentWords are the name words that mark a development mode, matched per
// word so that a name such as SESSION_MAX_ACTIVE_DEVICES — which merely contains
// the letters — is not one.
var developmentWords = map[string]bool{"DEV": true, "DEVELOPMENT": true}

// inMemoryWords are name words that state in-memory storage outright.
var inMemoryWords = map[string]bool{"INMEM": true, "INMEMORY": true}

// credentialWords are the name words that make a development-mode variable a
// credential of the store it opens, rather than a development convenience.
var credentialWords = map[string]bool{
	"TOKEN": true, "PASSWORD": true, "PASS": true, "SECRET": true,
	"KEY": true, "KEYS": true, "CREDENTIAL": true, "CREDENTIALS": true, "ROOT": true,
}

// storageWords name the variable that selects where a workload keeps its state.
var storageWords = map[string]bool{
	"STORAGE": true, "BACKEND": true, "STORE": true, "PERSISTENCE": true, "DATABASE": true,
}

// ephemeralStorageValues are the values of a storage variable that keep state
// only for the life of the process.
var ephemeralStorageValues = map[string]bool{
	"memory": true, "inmem": true, "in-memory": true, "in_memory": true,
	"inmemory": true, "ephemeral": true, "tmpfs": true, "none": true,
}

// Validate holds one decoded manifest document to every rule of the deployed
// posture, on behalf of the subject that rendered it. It returns the first
// violation as a *Violation, or nil when the document conforms or carries no
// workload at all.
//
// Rules are evaluated most specific first, so a TLS Secret mounted on a
// mesh-protected environment is reported as the transport-material rule it
// really breaks rather than as a generic mount.
func Validate(document map[string]any, subject Subject, location string, declaration *Declaration) error {
	kind := stringAt(document, "kind")
	name := metadataName(document)
	workload := kind
	if name != "" {
		workload = kind + "/" + name
	}
	for _, spec := range podSpecifications(document, kind) {
		report := func(rule, field, detail string) error {
			if _, allowed := declaration.Allows(rule, subject); allowed {
				return nil
			}
			return &Violation{
				Rule: rule, Subject: subject, Location: location,
				Workload: workload, Field: field, Detail: detail,
			}
		}
		if declaration.Asserted(AssertMeshProtectedTransport) {
			if err := checkPeerTransportMaterial(spec, report); err != nil {
				return err
			}
		}
		if err := checkVolumeSources(spec, report); err != nil {
			return err
		}
		if err := checkDevelopmentMode(spec, report); err != nil {
			return err
		}
	}
	return nil
}

// reporter turns a rule, a field path and a detail into a violation, or into nil
// when the environment allows that rule for this subject.
type reporter func(rule, field, detail string) error

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

// checkPeerTransportMaterial refuses a workload that configures its own
// certificate, key or CA material, or mounts a volume that delivers it. The
// environment has already said transport between its workloads is protected, so
// this material is either redundant or a second, unmanaged trust root.
func checkPeerTransportMaterial(spec pathedSpec, report reporter) error {
	detail := "the workload carries its own TLS material for its in-cell peers, " +
		"but the environment asserts " + AssertMeshProtectedTransport +
		": transport between workloads belongs to the mesh and TLS at the edge belongs to the ingress"
	for _, container := range containers(spec) {
		for _, entry := range container.environment() {
			if marker, found := transportMaterial(entry.name); found {
				return report(RulePeerTransportMaterial, entry.namePath,
					fmt.Sprintf("%s declares %s (%s)", entry.name, marker, detail))
			}
			if marker, found := transportMaterial(entry.value); found {
				return report(RulePeerTransportMaterial, entry.valuePath,
					fmt.Sprintf("%s points at %s (%s)", entry.name, marker, detail))
			}
		}
		for _, token := range container.arguments() {
			if marker, found := transportMaterial(token.value); found {
				return report(RulePeerTransportMaterial, token.path,
					fmt.Sprintf("the command line declares %s (%s)", marker, detail))
			}
		}
	}
	for _, volume := range volumes(spec) {
		if volume.source == "" {
			continue
		}
		if marker, found := volumeTransportMaterial(volume.name, volume.value[volume.source]); found {
			return report(RulePeerTransportMaterial, volume.sourcePath,
				fmt.Sprintf("volume %q delivers %s as files (%s)", volume.name, marker, detail))
		}
	}
	return nil
}

// checkVolumeSources refuses every workload volume that is neither scratch space
// nor a durable data claim.
func checkVolumeSources(spec pathedSpec, report reporter) error {
	for _, volume := range volumes(spec) {
		switch volume.source {
		case scratchVolumeSource, durableVolumeSource, "":
			continue
		}
		return report(RuleNonScratchMount, volume.sourcePath, fmt.Sprintf(
			"volume %q takes its contents from a %s source, and a deployed workload mounts only the standard scratch volume (%s) or a durable data claim (%s); "+
				"configuration and credentials reach a service as values and secrets in the environment variables the render already projects, so a mount means something reads a file the platform never delivers",
			volume.name, volume.source, scratchVolumeSource, durableVolumeSource))
	}
	return nil
}

// checkDevelopmentMode refuses a workload started in a development or in-memory
// mode. A store that keeps its keys in memory loses them on the next node
// replacement, and a development mode is exactly the mode that does.
func checkDevelopmentMode(spec pathedSpec, report reporter) error {
	detail := "a deployed runtime keeps its keys and state in a durable store, and a development or in-memory mode keeps them only " +
		"for the life of the process: the next restart or node replacement loses them"
	for _, container := range containers(spec) {
		for _, token := range container.arguments() {
			if developmentSwitch(token.value) {
				return report(RuleInMemoryStateStore, token.path,
					fmt.Sprintf("the command line passes %q, a development-mode switch; %s", token.value, detail))
			}
		}
		for _, entry := range container.environment() {
			names := nameWords(entry.name)
			switch {
			case anyWord(names, inMemoryWords):
				return report(RuleInMemoryStateStore, entry.namePath,
					fmt.Sprintf("%s selects in-memory storage; %s", entry.name, detail))
			case anyWord(names, developmentWords) && anyWord(names, credentialWords):
				return report(RuleInMemoryStateStore, entry.namePath,
					fmt.Sprintf("%s is a development-mode credential, so the workload runs its development server; %s", entry.name, detail))
			case anyWord(names, storageWords) && ephemeralStorageValues[strings.ToLower(strings.TrimSpace(entry.value))]:
				return report(RuleInMemoryStateStore, entry.valuePath,
					fmt.Sprintf("%s selects %q storage; %s", entry.name, entry.value, detail))
			}
		}
	}
	return nil
}

// developmentSwitch reports whether a command-line token is a development-mode
// flag: -dev, --dev, --development, or any flag whose first word is one of them
// (-dev-root-token-id, --dev-mode=true). A flag that merely starts with the same
// letters (--device) is not one.
func developmentSwitch(token string) bool {
	flag := strings.ToLower(strings.TrimSpace(token))
	if !strings.HasPrefix(flag, "-") {
		return false
	}
	flag = strings.TrimLeft(flag, "-")
	for _, word := range []string{"dev", "development"} {
		if flag == word || strings.HasPrefix(flag, word+"-") || strings.HasPrefix(flag, word+"=") {
			return true
		}
	}
	return false
}

// transportMaterial reports the marker a single string carries, if any. The
// longest matching marker is reported, so a name that carries several is named
// by the most specific one.
func transportMaterial(value string) (string, bool) {
	lowered := strings.ToLower(value)
	longest := ""
	for _, file := range transportMaterialFiles {
		if strings.Contains(lowered, file) && len(file) > len(longest) {
			longest = file
		}
	}
	collapsed := canonical(value)
	for _, marker := range transportMaterialMarkers {
		if strings.Contains(collapsed, marker) && len(marker) > len(longest) {
			longest = marker
		}
	}
	return longest, longest != ""
}

// volumeTransportMaterial reports the TLS material a volume delivers as files:
// the marker any string inside its source carries, or the TLS word its own name
// or its source's names carry.
func volumeTransportMaterial(name string, source any) (string, bool) {
	if marker, found := transportMaterialIn(source); found {
		return marker, true
	}
	for _, candidate := range append([]string{name}, nestedStrings(source)...) {
		for word := range nameWords(candidate) {
			if transportMaterialNameWords[word] {
				return strings.ToLower(word), true
			}
		}
	}
	return "", false
}

// transportMaterialIn reports the marker any string nested inside a value
// carries — a volume source's secret name, its items' keys and paths.
func transportMaterialIn(value any) (string, bool) {
	for _, text := range nestedStrings(value) {
		if marker, found := transportMaterial(text); found {
			return marker, true
		}
	}
	return "", false
}

// nestedStrings flattens every string a value holds, in a deterministic order.
func nestedStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case map[string]any:
		var texts []string
		for _, key := range sortedMapKeys(typed) {
			texts = append(texts, nestedStrings(typed[key])...)
		}
		return texts
	case []any:
		var texts []string
		for _, child := range typed {
			texts = append(texts, nestedStrings(child)...)
		}
		return texts
	}
	return nil
}

// pathedVolume is one entry of a pod specification's volumes list.
type pathedVolume struct {
	name       string
	source     string
	sourcePath string
	value      map[string]any
}

// volumes decodes a pod specification's volumes, naming each one's single
// source. Kubernetes allows exactly one source per volume; when several are
// present the first by name is reported, so the refusal is deterministic.
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

// pathedContainer is one container of a pod specification.
type pathedContainer struct {
	path  string
	value map[string]any
}

// containers decodes every container of a pod specification, in the order the
// three container lists are declared.
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

// environmentEntry is one env entry of a container, with the field paths of its
// name and its value.
type environmentEntry struct {
	name      string
	value     string
	namePath  string
	valuePath string
}

func (container pathedContainer) environment() []environmentEntry {
	list, ok := container.value["env"].([]any)
	if !ok {
		return nil
	}
	entries := make([]environmentEntry, 0, len(list))
	for index, entry := range list {
		value, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		entries = append(entries, environmentEntry{
			name:      stringAt(value, "name"),
			value:     stringAt(value, "value"),
			namePath:  fmt.Sprintf("%s.env[%d].name", container.path, index),
			valuePath: fmt.Sprintf("%s.env[%d].value", container.path, index),
		})
	}
	return entries
}

// argumentToken is one token of a container's command or args.
type argumentToken struct {
	value string
	path  string
}

// arguments decodes a container's command and args as individual tokens. Only
// the tokens are inspected: a shell body a container is handed as one argument
// is the agent's program, not a field a render can hold to a rule.
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

// canonical is the separator-free lowercase spelling a marker is matched
// against, so tls-cert-file, TLS_CERT_FILE and tlsCertFile are one name.
func canonical(value string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(value) {
		if unicode.IsLetter(symbol) || unicode.IsDigit(symbol) {
			builder.WriteRune(symbol)
		}
	}
	return builder.String()
}

// nameWords splits a configuration name into upper-case words, on separators and
// on camelCase boundaries. Matching per word is what keeps DEVICES from reading
// as a development marker.
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

func anyWord(words map[string]bool, markers map[string]bool) bool {
	for word := range words {
		if markers[word] {
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
