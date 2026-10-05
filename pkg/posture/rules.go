package posture

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// The manifest fields a pod specification is reached through.
const (
	specField        = "spec"
	templateField    = "template"
	jobTemplateField = "jobTemplate"
)

// configMapKind names a ConfigMap reference, the one reference kind an effective
// manifest set can resolve for itself.
const configMapKind = "configMap"

// wordBackend and wordStorage are the name words that select where state lives.
const (
	wordBackend = "BACKEND"
	wordStorage = "STORAGE"
)

// scratchVolumeSource is the standard scratch volume: writable space for a
// read-only root filesystem, carrying nothing the platform did not put there at
// start.
const scratchVolumeSource = "emptyDir"

// durableVolumeSource is a durable data claim: the storage a stateful workload
// keeps its own data in, which RuleInMemoryStateStore exists to require. It is
// admitted as data storage only, never as a way to deliver configuration or
// credentials — see configurationMountPaths.
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

// transportMaterialOptions name a setting that configures certificate, key or CA
// material, matched against the separator-free lowercase spelling so that
// --tls-cert-file, TLS_CERT_FILE and tlsCertFile are one name.
//
// Every entry either is transport vocabulary itself or carries the transport word
// that makes it so. A bare key name is deliberately absent — an application's own
// signing key or an API key read from a file is a credential, not its transport —
// and a name alone never decides: RuleTransportMaterial also requires the value to
// BE material (transportMaterialValue), so a credential named through a reference,
// with no value in the manifest, is not mistaken for a certificate.
var transportMaterialOptions = []string{
	"cabundle", "cacert", "cachain", "cafile", "cert", "certchain", "certfile",
	"certificate", "certificatechain", "certificatefile", "certificatepath", "certs",
	"clientca", "clientcert", "keystore", "mtlscert", "mtlskey", "peercert", "peerkey",
	"servercert", "serverkey", "sslca", "sslcert", "sslkey", "tlsca", "tlscert",
	"tlskey", "trustedca", "truststore",
}

// transportEnableOptions name a setting that turns a workload's own TLS on. The
// value decides: MUTUAL_TLS=false configures no transport at all.
var transportEnableOptions = []string{
	"enabletls", "enablessl", "mtls", "mutualtls", "requiretls", "ssl", "tls",
	"tlsenabled", "usetls", "usessl",
}

// listenerOptions name the setting that says what a workload serves on, by word,
// so that a value carrying an https scheme is read as that workload terminating
// TLS itself.
var listenerWords = map[string]bool{
	"LISTEN": true, "LISTENER": true, "BIND": true, "ADDR": true, "ADDRESS": true,
	"SERVE": true, "SERVER": true, "ENDPOINT": true, "URL": true, "ADVERTISE": true,
}

// materialExtensions are the file types certificate, key and CA material is
// delivered as.
var materialExtensions = []string{".crt", ".cer", ".pem", ".key", ".p12", ".pfx", ".jks", ".der"}

// transportMaterialFiles are the conventional names TLS material is mounted
// under, matched inside a volume source or a value.
var transportMaterialFiles = []string{"tls.crt", "tls.key", "ca.crt", "ca.pem", "cert.pem", "key.pem"}

// transportMaterialNameWords are the words that name TLS material in a volume,
// a Secret or a ConfigMap. They are matched per word and only inside a volume
// source: a word this short would read as material in too many other names.
var transportMaterialNameWords = map[string]bool{
	"TLS": true, "MTLS": true, "SSL": true, "CA": true, "PKI": true,
	"CERT": true, "CERTS": true, "CERTIFICATE": true, "CERTIFICATES": true,
}

// developmentWords mark a development mode, matched per word so that a name
// merely containing the letters (SESSION_MAX_ACTIVE_DEVICES) is not one.
var developmentWords = map[string]bool{"DEV": true, "DEVELOPMENT": true}

// developmentCompanionWords are the words that make a development marker in an
// ENVIRONMENT name a statement about how the workload runs, rather than part of
// some other name (a development portal's URL). A command-line option named dev
// needs no companion: that is what the option is.
var developmentCompanionWords = map[string]bool{
	"MODE": true, "SERVER": true, "ROOT": true, "TOKEN": true, "KEY": true,
	"KEYS": true, "SECRET": true, "PASSWORD": true, wordStorage: true, wordBackend: true,
	"STORE": true, "ENABLED": true, "ENABLE": true,
}

// stateWords name what a setting is about: where a workload keeps its own state.
var stateWords = map[string]bool{
	wordStorage: true, wordBackend: true, "STORE": true, "PERSISTENCE": true,
	"STATE": true, "DATABASE": true, "DB": true, "DATA": true,
}

// stateSelectorWords name a setting that SELECTS a mode rather than merely
// mentioning storage. Both halves are required, so STORAGE_BACKEND and --storage
// are read as the storage mode while STORE_ROOT_TOKEN and DATABASE_URL — which
// name no mode — are not, including when their values cannot be read.
var stateSelectorWords = map[string]bool{
	"MODE": true, wordBackend: true, "ENGINE": true, "TYPE": true, "DRIVER": true,
	"PROVIDER": true, "KIND": true, wordStorage: true, "PERSISTENCE": true,
}

// ephemeralStateValues are the values of a state setting that keep state only for
// the life of the process.
var ephemeralStateValues = map[string]bool{
	"memory": true, "inmem": true, "in-memory": true, "in_memory": true,
	"inmemory": true, "ram": true, "ephemeral": true, "tmpfs": true, "none": true,
}

// inMemoryOptions name a setting that selects in-memory storage outright.
var inMemoryOptions = []string{"inmem", "inmemory"}

// credentialWords and custodyWords together identify a workload that keeps
// credential material as its own state, rather than one merely handed a password
// of its own: a store's root or master credential mints or protects others, while
// an ordinary service's database password does neither. This is what keeps
// RuleInMemoryStateStore off an in-memory cache, which loses nothing that matters
// when it restarts.
var credentialWords = map[string]bool{
	"TOKEN": true, "TOKENS": true, "KEY": true, "KEYS": true, "SECRET": true,
	"SECRETS": true, "PASSWORD": true, "CREDENTIAL": true, "CREDENTIALS": true,
}

var custodyWords = map[string]bool{
	"ROOT": true, "MASTER": true, "ADMIN": true, "SIGNING": true, "ISSUER": true,
	"KEYRING": true, "CUSTODY": true, "UNLOCK": true, "RECOVERY": true,
}

// configurationMountPaths are the destinations a file-delivered configuration or
// credential lands at. A mount there is refused whatever its source, because the
// rule is about what reaches a workload as files, not about which Kubernetes
// volume type carried it: a scratch volume mounted over /secrets delivers the
// same thing a Secret volume would.
var configurationMountPaths = []string{
	"/etc", "/config", "/configs", "/conf", "/secrets", "/secret", "/credentials",
	"/keys", "/certs", "/cert", "/tls", "/pki", "/run/secrets", "/var/run/secrets",
}

// ValidateDocuments holds a whole effective manifest set to the deployed posture.
// It is the entry point every restricted render path uses, and the only one that
// can resolve a value a workload reads from elsewhere in the same set, so a rule
// is never satisfied merely by moving a value into a ConfigMap.
//
// Wrapper lists are expanded first: a workload shipped inside a List is applied
// exactly as a top-level workload is.
func ValidateDocuments(documents []Document, defaults Subject, declaration *Declaration) error {
	expanded := expand(documents)
	index := newReferenceIndex(expanded)
	for _, document := range expanded {
		subject := SubjectFromPath(document.Location, defaults)
		if err := validateDocument(document, subject, index, declaration); err != nil {
			return err
		}
	}
	return nil
}

// Validate holds one decoded manifest to the posture, with no manifest set around
// it to resolve references against. Prefer ValidateDocuments.
func Validate(document map[string]any, subject Subject, location string, declaration *Declaration) error {
	return ValidateDocuments([]Document{{Location: location, Value: document}}, subject, declaration)
}

// validateDocument applies every rule to one workload, most specific first, so a
// TLS Secret mounted on a mesh-protected environment is reported as the
// transport-material rule it really breaks rather than as a generic mount.
func validateDocument(document Document, subject Subject, index *referenceIndex, declaration *Declaration) error {
	kind := stringAt(document.Value, "kind")
	name := metadataName(document.Value)
	workload := kind
	if name != "" {
		workload = kind + "/" + name
	}
	for _, spec := range podSpecifications(document.Value, kind) {
		report := func(rule, field, detail string) error {
			if _, allowed := declaration.Allows(rule, subject); allowed {
				return nil
			}
			return &Violation{
				Rule: rule, Subject: subject, Location: document.Location,
				Workload: workload, Field: field, Detail: detail,
			}
		}
		configurations := make([]containerSettings, 0, 4)
		for _, container := range containers(spec) {
			configurations = append(configurations, containerConfiguration(container, index))
		}
		if declaration.Asserted(AssertMeshProtectedTransport) {
			if err := checkTransportMaterial(spec, configurations, report); err != nil {
				return err
			}
		}
		if err := checkMounts(spec, report); err != nil {
			return err
		}
		if err := checkEphemeralCredentialState(configurations, report); err != nil {
			return err
		}
	}
	return nil
}

// reporter turns a rule, a field path and a detail into a violation, or into nil
// when the environment allows that rule for this subject. A caller keeps looking
// after a nil: an allowance covers one rule for one service, so a workload allowed
// to mount a TLS Secret is still held to the mount rule.
type reporter func(rule, field, detail string) error

// checkTransportMaterial refuses a workload that terminates or initiates TLS
// itself — configured with certificate, key or CA material, serving an https
// listener, or mounting that material as files — on an environment that has
// already said transport between its workloads is protected.
func checkTransportMaterial(spec pathedSpec, configurations []containerSettings, report reporter) error {
	detail := "the workload configures its own TLS for its in-cell peers, but the environment asserts " +
		AssertMeshProtectedTransport +
		": transport between workloads belongs to the mesh and TLS at the edge belongs to the ingress"
	for index := range configurations {
		configuration := &configurations[index]
		for entryIndex := range configuration.settings {
			entry := &configuration.settings[entryIndex]
			switch {
			case transportMaterialSetting(entry):
				if err := report(RulePeerTransportMaterial, entry.field, fmt.Sprintf(
					"%s configures TLS material (%s)", entry.describe(), detail)); err != nil {
					return err
				}
			case httpsListener(entry):
				if err := report(RulePeerTransportMaterial, entry.field, fmt.Sprintf(
					"%s serves TLS itself (%s)", entry.describe(), detail)); err != nil {
					return err
				}
			case transportEnabled(entry):
				if err := report(RulePeerTransportMaterial, entry.field, fmt.Sprintf(
					"%s turns on the workload's own TLS (%s)", entry.describe(), detail)); err != nil {
					return err
				}
			}
		}
	}
	for _, volume := range volumes(spec) {
		if volume.source == "" {
			continue
		}
		if marker, found := volumeTransportMaterial(volume.name, volume.value[volume.source]); found {
			if err := report(RulePeerTransportMaterial, volume.sourcePath, fmt.Sprintf(
				"volume %q delivers %s as files (%s)", volume.name, marker, detail)); err != nil {
				return err
			}
		}
	}
	return nil
}

// transportMaterialSetting reports a setting that names TLS material AND carries
// material as its value. Both halves are required: the name alone would read an
// application's own signing key as a certificate, and the value alone would read
// every file path as one.
func transportMaterialSetting(entry *setting) bool {
	if !matchesAny(entry.canonical(), transportMaterialOptions) {
		return false
	}
	return transportMaterialValue(entry)
}

// transportMaterialValue reports whether a value is certificate, key or CA
// material: inline PEM, or a path to a file. An unresolved value under a material
// name is treated as material — the render cannot see it, and reading absence as
// conformance would make the rule optional.
func transportMaterialValue(entry *setting) bool {
	if !entry.resolved {
		return true
	}
	value := strings.TrimSpace(entry.value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "-----BEGIN") {
		return true
	}
	lowered := strings.ToLower(value)
	for _, extension := range materialExtensions {
		if strings.HasSuffix(lowered, extension) {
			return true
		}
	}
	for _, file := range transportMaterialFiles {
		if strings.Contains(lowered, file) {
			return true
		}
	}
	// A material option pointed at a file is material whatever the file is called.
	return strings.Contains(value, "/") && !strings.Contains(value, "://")
}

// httpsListener reports a setting that says this workload serves, or dials a
// peer over, TLS it terminates itself.
func httpsListener(entry *setting) bool {
	if !anyWord(entry.words(), listenerWords) {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.value)), "https://")
}

// transportEnabled reports a setting that turns the workload's own TLS on, read
// for truth so that a disabled one is not a violation.
func transportEnabled(entry *setting) bool {
	return exactlyAny(entry.canonical(), transportEnableOptions) && entry.enabled()
}

// checkMounts refuses what reaches a workload as files. A volume is admitted only
// as scratch space or as a durable data claim, and neither may be mounted where a
// configuration or a credential would be read from.
func checkMounts(spec pathedSpec, report reporter) error {
	byName := map[string]pathedVolume{}
	for _, volume := range volumes(spec) {
		byName[volume.name] = volume
		switch volume.source {
		case scratchVolumeSource, durableVolumeSource, "":
			continue
		}
		if err := report(RuleNonScratchMount, volume.sourcePath, fmt.Sprintf(
			"volume %q takes its contents from a %s source, and a deployed workload mounts only the standard scratch volume (%s) or a durable data claim (%s); "+
				"configuration and credentials reach a service as values and secrets in the environment variables the render already projects, so a mount means something reads a file the platform never delivers",
			volume.name, volume.source, scratchVolumeSource, durableVolumeSource)); err != nil {
			return err
		}
	}
	for _, mount := range mounts(spec) {
		if !configurationMountPath(mount.path) {
			continue
		}
		volume := byName[mount.volume]
		source := volume.source
		if source == "" {
			source = "unknown"
		}
		if err := report(RuleNonScratchMount, mount.field, fmt.Sprintf(
			"volume %q (a %s source) is mounted at %s, where a service reads its configuration or its credentials from files; "+
				"those reach a service as values and secrets in the environment variables the render already projects, whatever volume type carries them",
			mount.volume, source, mount.path)); err != nil {
			return err
		}
	}
	return nil
}

// checkEphemeralCredentialState refuses a workload that keeps credential material
// as its own state while keeping that state only for the life of the process.
//
// Both halves are required, and that is the rule rather than a softening of it: an
// in-memory cache loses nothing that cannot be recomputed, while a store whose
// keys live in memory loses, on one node replacement, everything encrypted under
// them. The custody half is what tells the two apart (see custodyWords).
//
// Where the workload keeps credential material and its startup program is a shell
// body the render cannot read as configuration, the render refuses rather than
// assume the program is conformant: the evidence has to be explicit.
func checkEphemeralCredentialState(configurations []containerSettings, report reporter) error {
	detail := "a deployed runtime keeps its keys and state in a durable store, and a development or in-memory mode keeps them only " +
		"for the life of the process: the next restart or node replacement loses them"
	for index := range configurations {
		configuration := &configurations[index]
		custody, _ := configuration.custodyOfCredentials()
		if custody == "" {
			continue
		}
		for entryIndex := range configuration.settings {
			entry := &configuration.settings[entryIndex]
			var reason string
			switch {
			case developmentMode(entry):
				reason = fmt.Sprintf("%s starts its development server", entry.describe())
			case ephemeralState(entry):
				reason = fmt.Sprintf("%s keeps its state in memory", entry.describe())
			case inMemorySelected(entry):
				reason = fmt.Sprintf("%s selects in-memory storage", entry.describe())
			default:
				continue
			}
			if err := report(RuleInMemoryStateStore, entry.field, fmt.Sprintf(
				"the workload holds credential material (%s) and %s; %s", custody, reason, detail)); err != nil {
				return err
			}
		}
		if configuration.opaqueProgram != "" {
			if err := report(RuleInMemoryStateStore, configuration.opaqueProgramField, fmt.Sprintf(
				"the workload holds credential material (%s) and is started by a shell program this render cannot read as configuration, "+
					"so it cannot show which storage mode that program selects; %s. "+
					"Start it with an explicit command and options, or declare the exception",
				custody, detail)); err != nil {
				return err
			}
		}
	}
	return nil
}

// custodyOfCredentials reports the setting by which a workload holds credential
// material as its own state — a root, master or signing credential, rather than
// the password an ordinary service uses to reach something else.
func (configuration *containerSettings) custodyOfCredentials() (string, string) {
	for index := range configuration.settings {
		entry := &configuration.settings[index]
		words := entry.words()
		if anyWord(words, credentialWords) && anyWord(words, custodyWords) {
			return entry.name, entry.field
		}
	}
	return "", ""
}

// developmentMode reports a setting that starts a development server. A
// command-line option named dev says so by itself; an environment name needs a
// companion word, so that a development portal's URL is not read as one.
func developmentMode(entry *setting) bool {
	words := entry.words()
	if !anyWord(words, developmentWords) {
		return false
	}
	if !entry.flag && !entry.resolved {
		return true
	}
	if !entry.flag && !anyWord(words, developmentCompanionWords) {
		return false
	}
	return entry.enabled()
}

// ephemeralState reports a setting that selects where state lives and chooses
// somewhere it does not survive the process.
func ephemeralState(entry *setting) bool {
	words := entry.words()
	if !anyWord(words, stateWords) || !anyWord(words, stateSelectorWords) {
		return false
	}
	if !entry.resolved {
		return true
	}
	return ephemeralStateValues[strings.ToLower(strings.TrimSpace(entry.value))]
}

// inMemorySelected reports a setting that names in-memory storage outright, read
// for truth so that one switched off is not a violation.
func inMemorySelected(entry *setting) bool {
	return matchesAny(entry.canonical(), inMemoryOptions) && entry.enabled()
}

// describe names a setting the way a refusal should: the name, and the value when
// the render could read one.
func (s *setting) describe() string {
	switch {
	case s.display != "":
		return s.display
	case !s.resolved:
		return fmt.Sprintf("%s (from %s, a value this render cannot read)", s.name, s.reference)
	case s.value == "":
		return s.name
	default:
		return fmt.Sprintf("%s=%s", s.name, s.value)
	}
}

func matchesAny(canonical string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(canonical, marker) {
			return true
		}
	}
	return false
}

func exactlyAny(canonical string, markers []string) bool {
	for _, marker := range markers {
		if canonical == marker {
			return true
		}
	}
	return false
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
				field:  fmt.Sprintf("%s.volumeMounts[%d].mountPath", container.path, index),
			})
		}
	}
	return decoded
}

// configurationMountPath reports whether a mount destination is where a service
// reads configuration or credentials from files.
func configurationMountPath(mountPath string) bool {
	clean := "/" + strings.Trim(strings.TrimSpace(mountPath), "/")
	for _, candidate := range configurationMountPaths {
		if clean == candidate || strings.HasPrefix(clean, candidate+"/") {
			return true
		}
	}
	return false
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
		if kind == "field" {
			name = stringAt(reference, "fieldPath")
		}
		if kind == "resource" {
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

// volumeTransportMaterial reports the TLS material a volume delivers as files.
func volumeTransportMaterial(name string, source any) (string, bool) {
	for _, text := range nestedStrings(source) {
		lowered := strings.ToLower(text)
		for _, file := range transportMaterialFiles {
			if strings.Contains(lowered, file) {
				return file, true
			}
		}
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
