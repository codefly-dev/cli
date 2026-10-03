// Package modulecontract reads the contract a module publishes beside its
// manifest — module.contract.codefly.yaml — and resolves it against a
// composition.
//
// The contract is a REQUEST, not a grant. It says what the module asks to be
// able to do on a host: the principal its credentials are issued to, the
// operation bindings it redeems, the queues and namespaces it declares, the
// scope ceilings it contributes, and the destinations it exposes. Three records
// narrow it in order — the host's envelope (the ceiling a platform
// administrator writes), the authority document the render derives from this
// file and the delivery pipeline signs, and an organisation's installation — so
// nothing here is wider than what the module exercises, and nothing here grants
// anything by itself.
//
// Four things never appear in a contract, because they are not the module's to
// assert: tenancy and cross-tenant reach (the envelope is the only record that
// may name an organisation), the approved build digest (the render's: the OCI
// image manifest digest of the container it pins), and the expected workload
// identity (the platform's: it issues SVIDs and owns the trust domain). The
// decoder is strict, so a contract that tries to carry one is refused rather
// than silently honoured.
//
// # Slots
//
// A binding's audience and the resource kind its scopes name are expressed in
// ANOTHER module's vocabulary, which a module repository must not spell. They
// are declared as slots — {from: <group>/<key>} — pointing at a workspace
// configuration value the composition supplies, and the render resolves them
// when it derives the authority document. A slot resolves PUBLIC configuration
// only: an audience and a resource kind are names, and a render carries secrets
// only as references, so a slot that resolves to a secret-classified value fails
// the derivation rather than inlining it into a delivered document. A bare
// string where a slot belongs is a schema error, not a value: a literal audience
// written into a module repository is a coupling across a module boundary that
// should never be accepted silently.
//
// A slot's KEY NAME carries its meaning, because no reader can check what a
// resolved value means: an audience slot pointing at a declared, supplied key
// that holds a model profile name resolves cleanly to a binding addressed to a
// profile, which the receiver refuses far from the contract that caused it. So
// the reader holds the convention the modules publish against: audience takes
// a *-audience key (or a *-prefix one, since a module's prefix is the audience
// a capability for it is addressed to), resource_kind a *-resource-kind key and
// binding_key a *-binding key, in either spelling core accepts.
//
// # Scope ceilings
//
// A binding's ceiling is written per operation in one of two spellings. Bare
// actions ([read, write]) are qualified by the binding's resource_kind slot,
// and a binding declaring none cannot write them: a scope names a resource
// kind, and one without a kind is a request nothing can mint. A binding whose
// acts span several kinds spells each scope out instead —
// [{resource_kind: other.things, actions: [write]}] — naming the kind
// literally, because a permission's namespace is fixed by the module that
// contributes it, exactly as its proto package is, while a service the
// composition chooses is configuration and stays a slot.
package modulecontract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// FileName is the contract's path relative to the module directory, beside
	// module.codefly.yaml and module.package.codefly.yaml. It is "module."-
	// prefixed because "contract" alone already means an API contract in this
	// ecosystem (contracts/api/catalog.codefly.json).
	FileName = "module.contract.codefly.yaml"

	// SchemaV1 is the only contract schema this package reads. It names the
	// thing, never the design note that argued for it: the note will be
	// superseded, the file outlives it, and the schema string is the one part
	// that cannot change without a version step.
	SchemaV1 = "codefly/module-contract/v1"
)

// Operations a binding may declare. invoke is a person's own capability,
// attenuated from a parent context the person caused to be minted; lookup is its
// read-only half; headless is the module's own capability with no person in it.
const (
	OperationInvoke   = "invoke"
	OperationLookup   = "lookup"
	OperationHeadless = "headless"
)

var operations = []string{OperationInvoke, OperationLookup, OperationHeadless}

// The slot fields of a binding, as the file spells them: what a refusal names
// and what the key-naming convention is keyed by.
const (
	fieldAudience     = "audience"
	fieldResourceKind = "resource_kind"
	fieldBindingKey   = "binding_key"
)

// Destination kinds. The vocabulary belongs with the host's envelope table;
// these three are what is written until that table names it.
const (
	DestinationModule           = "module"
	DestinationHost             = "host"
	DestinationPlatformInternal = "platform-internal"
)

var destinationKinds = []string{DestinationModule, DestinationHost, DestinationPlatformInternal}

var (
	// ErrSchema means the file does not declare a schema this package reads: a
	// version skew, not a malformed contract.
	ErrSchema = errors.New("module contract schema is not supported")
	// ErrInvalid means the contract violates its own rules.
	ErrInvalid = errors.New("module contract is invalid")
	// ErrUnresolvedSlot means a slot points at a workspace configuration value
	// the composition does not supply for this environment.
	ErrUnresolvedSlot = errors.New("module contract slot cannot be resolved")
	// ErrSecretSlot means a slot resolved to a secret-classified value. A render
	// carries secrets only as references, so the value is refused rather than
	// written into a delivered document.
	ErrSecretSlot = errors.New("module contract slot resolves to a secret")
)

var (
	// namePattern is one lowercase dotted or dashed segment: a principal, a
	// binding ID, a queue, a namespace, a resource kind, a service or an
	// endpoint name.
	namePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	// actionPattern is one action of a scope: lowercase, underscores allowed.
	actionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// slotKeyPattern is the key half of a slot: a workspace configuration key as
	// a composition writes it, in either spelling core accepts.
	slotKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// Contract is one module's published contract, as read from its file.
type Contract struct {
	// Schema is the document's version, checked first.
	Schema string `yaml:"schema"`
	// Principal is the principal every credential the module mints is issued
	// to: the module's own name.
	Principal string `yaml:"principal"`
	// Namespaces are the namespaces the module declares — the audit namespaces
	// it emits under, which an operator binds to its principal.
	Namespaces []string `yaml:"namespaces"`
	// Queues are the queues the module owns. Declared empty rather than
	// omitted: an absent list and "there are none" must not look the same.
	Queues []string `yaml:"queues"`
	// ScopeCeilings are the module's own permission vocabulary: the widest
	// authority any credential naming the module's resources may carry.
	ScopeCeilings []ScopeCeiling `yaml:"scope_ceilings"`
	// Bindings are the operation bindings the module redeems, outward.
	Bindings []Binding `yaml:"bindings"`
	// Destinations are the endpoints the module exposes for something else to
	// route to.
	Destinations []Destination `yaml:"destinations"`
}

// ScopeCeiling is one resource kind of the module's own vocabulary and the
// actions it contributes on it.
type ScopeCeiling struct {
	ResourceKind string   `yaml:"resource_kind"`
	Actions      []string `yaml:"actions"`
}

// Binding is one operation binding the module asks to redeem.
type Binding struct {
	// ID is the module's own stable id for the binding.
	ID string `yaml:"id"`
	// Revision is the binding's revision, at least 1 and 1 when omitted. A
	// credential seals it and a verifier holding another one refuses, so a
	// module bumps it when the binding's meaning changes.
	Revision uint64 `yaml:"revision,omitempty"`
	// Operations are the operations the binding needs: invoke, lookup, headless.
	Operations []string `yaml:"operations"`
	// Lookup carries the lookup method when the binding declares lookup.
	Lookup *Lookup `yaml:"lookup,omitempty"`
	// Audience is the operation audience, a slot into the composition.
	Audience Slot `yaml:"audience"`
	// ResourceKind is the other module's resource kind the scopes name, a
	// slot. Absent for a binding whose scopes are bare actions.
	ResourceKind *Slot `yaml:"resource_kind,omitempty"`
	// BindingKey is the key the host installs the binding under, a slot.
	BindingKey *Slot `yaml:"binding_key,omitempty"`
	// ScopeCeiling is, per declared operation, the scopes the binding may at
	// most carry. Every declared operation names its ceiling: a binding
	// declaring no scopes for an operation cannot be minted that way at all.
	ScopeCeiling map[string]Ceiling `yaml:"scope_ceiling"`
}

// Ceiling is one operation's scope ceiling, in exactly one of two spellings:
// bare actions qualified by the binding's resource_kind slot, or explicit
// scopes each naming its resource kind literally. A sequence mixing the two is
// refused, so a reader never has to guess which kind an action was meant for.
type Ceiling struct {
	// Actions is the bare spelling: [read, write].
	Actions []string
	// Scopes is the explicit spelling: [{resource_kind: k, actions: [...]}].
	Scopes []CeilingScope
}

// CeilingScope is one explicit scope of a ceiling: a resource kind, named
// literally, and the actions permitted on it.
type CeilingScope struct {
	ResourceKind string   `yaml:"resource_kind"`
	Actions      []string `yaml:"actions"`
}

// UnmarshalYAML reads a ceiling in either spelling, deciding by the first
// element and refusing a sequence that changes shape after it.
func (ceiling *Ceiling) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("%w: a scope ceiling is a list of actions or of {resource_kind, actions} entries", ErrInvalid)
	}
	if len(node.Content) == 0 {
		return nil
	}
	switch node.Content[0].Kind {
	case yaml.ScalarNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("%w: a scope ceiling mixes bare actions with {resource_kind, actions} entries", ErrInvalid)
			}
			ceiling.Actions = append(ceiling.Actions, item.Value)
		}
	case yaml.MappingNode:
		for _, item := range node.Content {
			if item.Kind != yaml.MappingNode {
				return fmt.Errorf("%w: a scope ceiling mixes {resource_kind, actions} entries with bare actions", ErrInvalid)
			}
			for index := 0; index < len(item.Content); index += 2 {
				if key := item.Content[index].Value; key != fieldResourceKind && key != "actions" {
					return fmt.Errorf("%w: unknown scope ceiling field %q (an entry carries resource_kind and actions)", ErrInvalid, key)
				}
			}
			var scope CeilingScope
			if err := item.Decode(&scope); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			ceiling.Scopes = append(ceiling.Scopes, scope)
		}
	default:
		return fmt.Errorf("%w: a scope ceiling is a list of actions or of {resource_kind, actions} entries", ErrInvalid)
	}
	return nil
}

// empty reports a ceiling that permits nothing.
func (ceiling Ceiling) empty() bool { return len(ceiling.Actions) == 0 && len(ceiling.Scopes) == 0 }

// Lookup is how a binding's lookup operation discovers what it looks up.
type Lookup struct {
	Method string `yaml:"method"`
}

// Slot is a reference to a workspace configuration value the composition
// supplies: {from: <group>/<key>}. It is a struct and not a string so that a
// literal value where a slot belongs is a schema error at the reader.
type Slot struct {
	From string `yaml:"from"`
}

// SlotGroups lists the workspace configuration groups the contract's slots
// resolve from, each once, sorted: what a render of the module bakes into its
// delivered authority document beside what its services consume.
func (contract *Contract) SlotGroups() []string {
	seen := map[string]struct{}{}
	for _, binding := range contract.Bindings {
		for _, slot := range []*Slot{&binding.Audience, binding.ResourceKind, binding.BindingKey} {
			if slot == nil || slot.From == "" {
				continue
			}
			seen[slot.Group()] = struct{}{}
		}
	}
	groups := make([]string, 0, len(seen))
	for group := range seen {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}

// UnmarshalYAML refuses anything that is not a mapping with exactly the key
// "from": a bare scalar is a literal written where another module's vocabulary
// belongs, and an extra key is a slot trying to carry a default.
func (slot *Slot) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%w: a slot is {from: <group>/<key>}, not the literal %q; another module's vocabulary is supplied by the composition, never spelled in a module repository", ErrInvalid, node.Value)
	}
	for index := 0; index < len(node.Content); index += 2 {
		if key := node.Content[index].Value; key != "from" {
			return fmt.Errorf("%w: unknown slot field %q (a slot carries only from)", ErrInvalid, key)
		}
	}
	type plain Slot
	return node.Decode((*plain)(slot))
}

// Group and Key split the slot's reference into the workspace configuration
// group and the key within it.
func (slot Slot) Group() string { group, _, _ := strings.Cut(slot.From, "/"); return group }

// Key is the key half of the slot's reference.
func (slot Slot) Key() string { _, key, _ := strings.Cut(slot.From, "/"); return key }

// slotSuffixes is the naming convention a slot's key carries, per slot: the
// key's meaning, since no reader can check what the value it resolves to means.
// Compared in core's normalized spelling, so model-audience and MODEL_AUDIENCE
// both pass.
var slotSuffixes = map[string][]string{
	fieldAudience:     {"_AUDIENCE", "_PREFIX"},
	fieldResourceKind: {"_RESOURCE_KIND"},
	fieldBindingKey:   {"_BINDING"},
}

func (slot Slot) validate(label, field string) error {
	group, key, found := strings.Cut(slot.From, "/")
	if !found || !namePattern.MatchString(group) || !slotKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: %s slot %q is not <group>/<key>", ErrInvalid, label, slot.From)
	}
	suffixes := slotSuffixes[field]
	if !slices.ContainsFunc(suffixes, func(suffix string) bool { return strings.HasSuffix(normalizeKey(key), suffix) }) {
		spelled := make([]string, 0, len(suffixes))
		for _, suffix := range suffixes {
			spelled = append(spelled, "*"+strings.ToLower(strings.ReplaceAll(suffix, "_", "-")))
		}
		return fmt.Errorf("%w: %s slot %q names a key that is not %s; a slot's key carries its meaning, because no reader can check what the value it resolves to means",
			ErrInvalid, label, slot.From, strings.Join(spelled, " or "))
	}
	return nil
}

// Destination is one endpoint the module exposes for a caller outside it.
type Destination struct {
	ID       string `yaml:"id"`
	Service  string `yaml:"service"`
	Endpoint string `yaml:"endpoint"`
	Kind     string `yaml:"kind"`
}

// Load reads the contract of the module at moduleDir. A module that publishes
// no contract returns os.ErrNotExist, which a caller distinguishes from a
// contract that cannot be read: the first is a module with nothing to ask, the
// second is a module whose request is unreadable.
func Load(moduleDir string) (*Contract, error) {
	data, err := os.ReadFile(filepath.Join(moduleDir, FileName)) //nolint:gosec // the module directory the composition resolved, under a constant file name
	if err != nil {
		return nil, err
	}
	contract, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.ToSlash(filepath.Join(moduleDir, FileName)), err)
	}
	return contract, nil
}

// Parse decodes and validates one contract. Decoding is strict: an unknown
// field is an error rather than a silently ignored intention, which is what
// refuses a contract carrying a tenancy, a build digest or an identity it is not
// entitled to assert. The schema is checked first and leniently, so a contract
// of another version is reported as a version skew rather than as malformed.
func Parse(data []byte) (*Contract, error) {
	var header struct {
		Schema string `yaml:"schema"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if header.Schema != SchemaV1 {
		return nil, fmt.Errorf("%w: %q (this CLI reads %q)", ErrSchema, header.Schema, SchemaV1)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var contract Contract
	if err := decoder.Decode(&contract); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: the file holds more than one document", ErrInvalid)
	}
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	return &contract, nil
}

// Validate checks everything a contract can be checked against on its own.
func (contract *Contract) Validate() error {
	if contract == nil {
		return fmt.Errorf("%w: contract is required", ErrInvalid)
	}
	if contract.Schema != SchemaV1 {
		return fmt.Errorf("%w: %q (this CLI reads %q)", ErrSchema, contract.Schema, SchemaV1)
	}
	if !namePattern.MatchString(contract.Principal) {
		return fmt.Errorf("%w: principal %q is not a lowercase name", ErrInvalid, contract.Principal)
	}
	for _, list := range []struct {
		label  string
		values []string
	}{{"namespaces", contract.Namespaces}, {"queues", contract.Queues}} {
		if list.values == nil {
			return fmt.Errorf("%w: %s must be declared, empty when there are none; an absent list and \"there are none\" must not look the same", ErrInvalid, list.label)
		}
		if err := uniqueNames(list.label, list.values); err != nil {
			return err
		}
	}
	if err := contract.validateScopeCeilings(); err != nil {
		return err
	}
	if err := contract.validateBindings(); err != nil {
		return err
	}
	return contract.validateDestinations()
}

func uniqueNames(label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !namePattern.MatchString(value) {
			return fmt.Errorf("%w: %s entry %q is not a lowercase name", ErrInvalid, label, value)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%w: %s entry %q is declared twice", ErrInvalid, label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func (contract *Contract) validateScopeCeilings() error {
	seen := make(map[string]struct{}, len(contract.ScopeCeilings))
	for _, ceiling := range contract.ScopeCeilings {
		if !namePattern.MatchString(ceiling.ResourceKind) {
			return fmt.Errorf("%w: scope ceiling resource kind %q is not a lowercase name", ErrInvalid, ceiling.ResourceKind)
		}
		if _, exists := seen[ceiling.ResourceKind]; exists {
			return fmt.Errorf("%w: scope ceiling %q is declared twice", ErrInvalid, ceiling.ResourceKind)
		}
		seen[ceiling.ResourceKind] = struct{}{}
		if len(ceiling.Actions) == 0 {
			return fmt.Errorf("%w: scope ceiling %q contributes no action", ErrInvalid, ceiling.ResourceKind)
		}
		if err := validateActions("scope ceiling "+ceiling.ResourceKind, ceiling.Actions); err != nil {
			return err
		}
	}
	return nil
}

func validateActions(label string, actions []string) error {
	seen := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if !actionPattern.MatchString(action) {
			return fmt.Errorf("%w: %s action %q is not a lowercase action name", ErrInvalid, label, action)
		}
		if _, exists := seen[action]; exists {
			return fmt.Errorf("%w: %s action %q is declared twice", ErrInvalid, label, action)
		}
		seen[action] = struct{}{}
	}
	return nil
}

func (contract *Contract) validateBindings() error {
	seen := make(map[string]struct{}, len(contract.Bindings))
	for _, binding := range contract.Bindings {
		if !namePattern.MatchString(binding.ID) {
			return fmt.Errorf("%w: binding id %q is not a lowercase name", ErrInvalid, binding.ID)
		}
		if _, exists := seen[binding.ID]; exists {
			return fmt.Errorf("%w: binding %q is declared twice", ErrInvalid, binding.ID)
		}
		seen[binding.ID] = struct{}{}
		if len(binding.Operations) == 0 {
			return fmt.Errorf("%w: binding %q declares no operation", ErrInvalid, binding.ID)
		}
		declared := make(map[string]struct{}, len(binding.Operations))
		for _, operation := range binding.Operations {
			if !slices.Contains(operations, operation) {
				return fmt.Errorf("%w: binding %q operation %q is not one of %s", ErrInvalid, binding.ID, operation, strings.Join(operations, ", "))
			}
			if _, exists := declared[operation]; exists {
				return fmt.Errorf("%w: binding %q declares operation %q twice", ErrInvalid, binding.ID, operation)
			}
			declared[operation] = struct{}{}
		}
		if binding.Lookup != nil {
			if _, lookup := declared[OperationLookup]; !lookup {
				return fmt.Errorf("%w: binding %q declares a lookup method without the lookup operation", ErrInvalid, binding.ID)
			}
			if !namePattern.MatchString(binding.Lookup.Method) {
				return fmt.Errorf("%w: binding %q lookup method %q is not a lowercase name", ErrInvalid, binding.ID, binding.Lookup.Method)
			}
		}
		if err := binding.Audience.validate("binding "+binding.ID+" "+fieldAudience, fieldAudience); err != nil {
			return err
		}
		for _, slot := range []struct {
			label string
			slot  *Slot
		}{{fieldResourceKind, binding.ResourceKind}, {fieldBindingKey, binding.BindingKey}} {
			if slot.slot == nil {
				continue
			}
			if err := slot.slot.validate("binding "+binding.ID+" "+slot.label, slot.label); err != nil {
				return err
			}
		}
		for operation := range binding.ScopeCeiling {
			if _, exists := declared[operation]; !exists {
				return fmt.Errorf("%w: binding %q names a scope ceiling for %q, an operation it does not declare", ErrInvalid, binding.ID, operation)
			}
		}
		for _, operation := range binding.Operations {
			if err := binding.validateCeiling(operation); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateCeiling checks one operation's ceiling: present, and in a spelling
// the binding can qualify.
func (binding *Binding) validateCeiling(operation string) error {
	label := "binding " + binding.ID + " " + operation
	ceiling := binding.ScopeCeiling[operation]
	if ceiling.empty() {
		// The ceiling is the gate: a binding carrying no scopes for an
		// operation cannot be minted that way at all, so an operation
		// declared without one is a request for nothing.
		return fmt.Errorf("%w: binding %q declares operation %q with no scope ceiling", ErrInvalid, binding.ID, operation)
	}
	if len(ceiling.Actions) > 0 {
		if binding.ResourceKind == nil {
			// A scope names a resource kind (core's WorkScopeV1 requires one),
			// so a bare action with no kind to qualify it is a request nothing
			// can mint.
			return fmt.Errorf("%w: %s ceiling lists bare actions but the binding declares no resource_kind slot to qualify them; "+
				"add the slot, or spell each scope as {resource_kind: <kind>, actions: [...]}", ErrInvalid, label)
		}
		return validateActions(label, ceiling.Actions)
	}
	seen := make(map[string]struct{}, len(ceiling.Scopes))
	for _, scope := range ceiling.Scopes {
		if !namePattern.MatchString(scope.ResourceKind) {
			return fmt.Errorf("%w: %s ceiling resource kind %q is not a lowercase name", ErrInvalid, label, scope.ResourceKind)
		}
		if _, exists := seen[scope.ResourceKind]; exists {
			return fmt.Errorf("%w: %s ceiling names resource kind %q twice", ErrInvalid, label, scope.ResourceKind)
		}
		seen[scope.ResourceKind] = struct{}{}
		if len(scope.Actions) == 0 {
			return fmt.Errorf("%w: %s ceiling resource kind %q permits no action", ErrInvalid, label, scope.ResourceKind)
		}
		if err := validateActions(label+" "+scope.ResourceKind, scope.Actions); err != nil {
			return err
		}
	}
	return nil
}

func (contract *Contract) validateDestinations() error {
	seen := make(map[string]struct{}, len(contract.Destinations))
	for _, destination := range contract.Destinations {
		if !namePattern.MatchString(destination.ID) {
			return fmt.Errorf("%w: destination id %q is not a lowercase name", ErrInvalid, destination.ID)
		}
		if _, exists := seen[destination.ID]; exists {
			return fmt.Errorf("%w: destination %q is declared twice", ErrInvalid, destination.ID)
		}
		seen[destination.ID] = struct{}{}
		for _, part := range []struct{ label, value string }{{"service", destination.Service}, {"endpoint", destination.Endpoint}} {
			if !namePattern.MatchString(part.value) {
				return fmt.Errorf("%w: destination %q %s %q is not a lowercase name", ErrInvalid, destination.ID, part.label, part.value)
			}
		}
		if !slices.Contains(destinationKinds, destination.Kind) {
			return fmt.Errorf("%w: destination %q kind %q is not one of %s", ErrInvalid, destination.ID, destination.Kind, strings.Join(destinationKinds, ", "))
		}
	}
	return nil
}

// Values is where slots resolve from: the workspace configuration values the
// composition supplies for one environment. A lookup answers whether the group
// and key exist and whether the value is secret-classified; the key is matched
// in either spelling core accepts (model-profile, MODEL_PROFILE).
type Values interface {
	Value(group, key string) (value string, secret bool, found bool)
}

// Resolved is a contract with every slot resolved to the value the composition
// supplies, ready to be derived into an authority document.
type Resolved struct {
	Principal     string
	Namespaces    []string
	Queues        []string
	ScopeCeilings []ScopeCeiling
	Bindings      []ResolvedBinding
	Destinations  []Destination
}

// ResolvedBinding is one binding with its audience, resource kind and binding
// key resolved.
type ResolvedBinding struct {
	ID         string
	Revision   uint64
	Operations []string
	Lookup     *Lookup
	Audience   string
	// ResourceKind is the resolved resource kind, or empty when the binding
	// declared none.
	ResourceKind string
	// BindingKey is the resolved key, or empty when the binding declared none.
	BindingKey string
	// Scopes are, per operation, the scope strings the binding may at most
	// carry, every one "<resource kind>:<action>": the binding's resolved
	// resource kind qualifies its bare actions, and an explicit scope names
	// its own. Sorted.
	Scopes map[string][]string
}

// Resolve resolves every slot of the contract against values. Every unresolved
// or secret slot is reported in one error, so a composition is fixed in one
// pass rather than one slot per render.
func (contract *Contract) Resolve(values Values) (*Resolved, error) {
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	resolved := &Resolved{
		Principal:     contract.Principal,
		Namespaces:    slices.Clone(contract.Namespaces),
		Queues:        slices.Clone(contract.Queues),
		ScopeCeilings: slices.Clone(contract.ScopeCeilings),
		Destinations:  slices.Clone(contract.Destinations),
	}
	var unresolved, secret []string
	resolve := func(label string, slot *Slot) string {
		if slot == nil {
			return ""
		}
		value, isSecret, found := values.Value(slot.Group(), slot.Key())
		switch {
		case !found:
			unresolved = append(unresolved, label+" ← "+slot.From)
			return ""
		case isSecret:
			secret = append(secret, label+" ← "+slot.From)
			return ""
		case strings.TrimSpace(value) == "" || strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f }):
			unresolved = append(unresolved, label+" ← "+slot.From+" (the value is empty or not a single-line name)")
			return ""
		}
		return value
	}
	for _, binding := range contract.Bindings {
		revision := binding.Revision
		if revision == 0 {
			revision = 1
		}
		entry := ResolvedBinding{
			ID: binding.ID, Revision: revision, Operations: slices.Clone(binding.Operations), Lookup: binding.Lookup,
			Audience:     resolve("binding "+binding.ID+" audience", &binding.Audience),
			ResourceKind: resolve("binding "+binding.ID+" resource_kind", binding.ResourceKind),
			BindingKey:   resolve("binding "+binding.ID+" binding_key", binding.BindingKey),
			Scopes:       make(map[string][]string, len(binding.Operations)),
		}
		for _, operation := range binding.Operations {
			ceiling := binding.ScopeCeiling[operation]
			scopes := make([]string, 0, len(ceiling.Actions)+len(ceiling.Scopes))
			// Bare actions are qualified by the binding's resolved resource
			// kind; explicit scopes carry their own.
			for _, action := range ceiling.Actions {
				scopes = append(scopes, entry.ResourceKind+":"+action)
			}
			for _, scope := range ceiling.Scopes {
				for _, action := range scope.Actions {
					scopes = append(scopes, scope.ResourceKind+":"+action)
				}
			}
			sort.Strings(scopes)
			entry.Scopes[operation] = scopes
		}
		resolved.Bindings = append(resolved.Bindings, entry)
	}
	if len(secret) > 0 {
		return nil, fmt.Errorf("%w: %s; a slot resolves public configuration only, because its value is written into a delivered document", ErrSecretSlot, strings.Join(secret, "; "))
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("%w: %s; the composition supplies each as a workspace configuration value for this environment", ErrUnresolvedSlot, strings.Join(unresolved, "; "))
	}
	return resolved, nil
}

// MapValues is a Values over nested maps: group → key → value, with the keys
// of secrets listed separately. Keys match in either spelling core accepts.
type MapValues struct {
	Public  map[string]map[string]string
	Secrets map[string]map[string]string
}

// Value implements Values.
func (values MapValues) Value(group, key string) (string, bool, bool) {
	if value, found := lookupKey(values.Secrets[group], key); found {
		return value, true, true
	}
	value, found := lookupKey(values.Public[group], key)
	return value, false, found
}

// lookupKey finds key in values in either spelling core accepts: as written, or
// normalized the way core names a configuration key (upper case, dashes to
// underscores).
func lookupKey(values map[string]string, key string) (string, bool) {
	if value, found := values[key]; found {
		return value, true
	}
	normalized := normalizeKey(key)
	for candidate, value := range values {
		if normalizeKey(candidate) == normalized {
			return value, true
		}
	}
	return "", false
}

func normalizeKey(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}
