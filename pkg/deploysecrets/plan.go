// Package deploysecrets derives what an environment's secret store must hold for
// a rendered environment to materialize, and writes it: `codefly deploy
// secrets`.
//
// A render never carries a secret value. It names, in each service's projected
// ExternalSecret, the remote key and property every secret it reads resolves
// from, and leaves the store to hold them. This package reads those names back
// from the render and resolves each one to a source, in order:
//
//  1. keep — the store already holds it; a value is never rotated here;
//  2. derive — a federation credential or an encoding of several
//     (solutionrun.DeployedFederationSecrets): a solution's registration secret,
//     a consuming backend's `prefix:secret` map, a consumed module's identity
//     secret, and the registrar's digests of all of them, recomputed from the
//     plaintexts so the two ends always agree;
//  3. propagate — a configuration value is one value however many services read
//     it, so a key another remote secret already holds is copied from there;
//  4. generate — a key the environment's `service-secrets.generate` declares
//     random;
//  5. require — anything else is supplied from outside, and named.
//
// Values never leave the package: a plan reports property names and sources
// only, and the writes it carries are unexported.
package deploysecrets

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/secretgen"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
)

// Action is what a plan does for one stored property.
type Action string

const (
	// ActionKeep: the store holds the property, and it is left as it is.
	ActionKeep Action = "keep"
	// ActionDerive: the property is absent and is derived from federation
	// credentials.
	ActionDerive Action = "derive"
	// ActionUpdate: a derived property is present but no longer encodes the
	// credentials it derives from — a registrar missing a solution's digest — and
	// is rewritten. Only derived properties are ever updated.
	ActionUpdate Action = "update"
	// ActionPropagate: the property is absent here and copied from the remote key
	// that already holds the same configuration value.
	ActionPropagate Action = "propagate"
	// ActionGenerate: the property is absent and generated at random, as the
	// environment declares.
	ActionGenerate Action = "generate"
	// ActionRequire: the property is absent and nothing can produce it; the
	// operator supplies it.
	ActionRequire Action = "require"
	// ActionUnverified: the remote key exists but was not read, so whether it
	// holds the property is unknown. Source says what would happen if it does not.
	ActionUnverified Action = "unverified"
)

// PropertyPlan is the plan for one property of one remote key. It carries no
// value.
type PropertyPlan struct {
	Property string
	Action   Action
	Source   string
}

// SecretPlan is the plan for one remote key.
type SecretPlan struct {
	RemoteKey  string
	Services   []string
	Exists     bool
	HasVersion bool
	// Read is whether the plan knows the key's properties: it was read, or it
	// has nothing to read.
	Read       bool
	Properties []PropertyPlan
}

// CredentialPlan is where one federation credential's value comes from.
type CredentialPlan struct {
	Credential solutionrun.Credential
	// Action is keep (held in the store), generate (minted now) or unverified.
	Action Action
	Source string
}

// Plan is a resolved environment. Its exported fields name keys and sources
// only; the values it would write stay inside it.
type Plan struct {
	Store       string
	Secrets     []SecretPlan
	Credentials []CredentialPlan
	Notes       []string

	writes []pendingWrite
}

type pendingWrite struct {
	key      string
	document map[string]string
	create   bool
	// digests marks a write carrying a registrar's digests: it is applied after
	// every write carrying the plaintexts those digests admit.
	digests bool
}

// Changes lists the remote keys an apply would write, in the order it would
// write them.
func (plan *Plan) Changes() []string {
	keys := make([]string, 0, len(plan.writes))
	for _, write := range plan.writes {
		keys = append(keys, write.key)
	}
	return keys
}

// Required lists, as `remote-key#property`, every property the operator must
// supply.
func (plan *Plan) Required() []string {
	var required []string
	for _, secret := range plan.Secrets {
		for _, property := range secret.Properties {
			if property.Action == ActionRequire {
				required = append(required, secret.RemoteKey+"#"+property.Property)
			}
		}
	}
	return required
}

// Unverified reports whether any key could not be read, so the plan cannot be
// applied.
func (plan *Plan) Unverified() bool {
	for _, secret := range plan.Secrets {
		if !secret.Read {
			return true
		}
	}
	return false
}

// Inputs is everything a plan is resolved from.
type Inputs struct {
	Rendered   gitops.RenderedEnvironment
	Federation solutionrun.DeployedSecrets
	Generators []environments.EnvironmentSecretGenerator
	// Services are the workspace's service uniques, which a service-scoped
	// generator naming no services covers.
	Services []string
	Store    Store
	// ReadPayloads reads every existing remote key, in memory, to know which
	// properties it holds. Without it the plan is metadata only: it knows which
	// keys exist, not what they hold, and cannot be applied.
	ReadPayloads bool
}

// remoteState is one remote key as the plan sees it.
type remoteState struct {
	secret      gitops.RenderedServiceSecret
	description Description
	// document is nil when the key exists and was not read.
	document map[string]string
}

func (state *remoteState) known() bool { return state.document != nil }

// Build resolves every property the rendered environment reads to a source.
func Build(ctx context.Context, in *Inputs) (*Plan, error) {
	plan := &Plan{Store: in.Store.Name()}
	for _, note := range in.Federation.Notes {
		plan.Notes = append(plan.Notes, note.Message)
	}

	states := make([]*remoteState, 0, len(in.Rendered.Secrets))
	for _, secret := range in.Rendered.Secrets {
		if slices.Contains(secret.Properties, "") {
			return nil, fmt.Errorf("remote key %s is read as a bare value; only a JSON document read by property can be planned", secret.RemoteKey)
		}
		description, err := in.Store.Describe(ctx, secret.RemoteKey)
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", secret.RemoteKey, err)
		}
		state := &remoteState{secret: secret, description: description}
		switch {
		case !description.Exists || !description.HasVersion:
			state.document = map[string]string{}
		case in.ReadPayloads:
			document, err := in.Store.Read(ctx, secret.RemoteKey)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", secret.RemoteKey, err)
			}
			state.document = document
		}
		states = append(states, state)
	}

	derivations, err := federationDerivations(states, in.Federation)
	if err != nil {
		return nil, err
	}
	credentials, err := resolveCredentials(plan, states, derivations, in.Federation)
	if err != nil {
		return nil, err
	}
	generators, err := generatorIndex(in.Generators, in.Services)
	if err != nil {
		return nil, err
	}

	// generated holds one value per stored configuration key, so every remote key
	// reading it receives the same one.
	generated := map[string]string{}
	for _, state := range states {
		secretPlan := SecretPlan{
			RemoteKey:  state.secret.RemoteKey,
			Services:   state.secret.Services,
			Exists:     state.description.Exists,
			HasVersion: state.description.HasVersion,
			Read:       state.known(),
		}
		changes := map[string]string{}
		digests := false
		for _, property := range state.secret.Properties {
			var propertyPlan PropertyPlan
			var value string
			if derivation, derived := derivations[state.secret.RemoteKey][property]; derived {
				propertyPlan, value = planDerived(state, property, derivation, credentials)
				digests = digests || (derivation.Digest && value != "")
			} else {
				propertyPlan, value, err = planConfigured(state, property, states, generators, generated)
				if err != nil {
					return nil, err
				}
			}
			if value != "" {
				changes[property] = value
			}
			secretPlan.Properties = append(secretPlan.Properties, propertyPlan)
		}
		plan.Secrets = append(plan.Secrets, secretPlan)
		if len(changes) > 0 && state.known() {
			document := maps.Clone(state.document)
			maps.Copy(document, changes)
			plan.writes = append(plan.writes, pendingWrite{key: state.secret.RemoteKey, document: document, create: !state.description.Exists, digests: digests})
		}
	}
	sort.SliceStable(plan.writes, func(i, j int) bool { return !plan.writes[i].digests && plan.writes[j].digests })
	return plan, nil
}

// federationDerivations maps each remote key's properties to the federation
// derivation of the service reading it.
func federationDerivations(states []*remoteState, federation solutionrun.DeployedSecrets) (map[string]map[string]solutionrun.SecretDerivation, error) {
	derivations := map[string]map[string]solutionrun.SecretDerivation{}
	for _, state := range states {
		for _, property := range state.secret.Properties {
			var found *solutionrun.SecretDerivation
			for _, unique := range state.secret.Services {
				derivation, ok := federation.For(unique, property)
				if !ok {
					continue
				}
				if found != nil && !slices.Equal(found.Credentials, derivation.Credentials) {
					return nil, fmt.Errorf("remote key %s property %s is read by services deriving it from different credentials", state.secret.RemoteKey, property)
				}
				found = &derivation
			}
			if found == nil {
				continue
			}
			if derivations[state.secret.RemoteKey] == nil {
				derivations[state.secret.RemoteKey] = map[string]solutionrun.SecretDerivation{}
			}
			derivations[state.secret.RemoteKey][property] = *found
		}
	}
	return derivations, nil
}

// credentialValues are the federation credentials' plaintexts, and which could
// not be established because a key that may hold one was not read.
type credentialValues struct {
	plaintexts map[solutionrun.Credential]string
	unverified map[solutionrun.Credential]bool
}

// resolveCredentials establishes every federation credential: the plaintext the
// store already holds, recovered from whichever key carries it, else a freshly
// minted one. Two keys carrying one credential with different values is a store
// that no longer federates, and is refused rather than silently resolved.
func resolveCredentials(plan *Plan, states []*remoteState, derivations map[string]map[string]solutionrun.SecretDerivation, federation solutionrun.DeployedSecrets) (credentialValues, error) {
	values := credentialValues{plaintexts: map[solutionrun.Credential]string{}, unverified: map[solutionrun.Credential]bool{}}
	heldIn := map[solutionrun.Credential]string{}
	for _, state := range states {
		for property, derivation := range derivations[state.secret.RemoteKey] {
			if derivation.Digest {
				continue
			}
			if !state.known() {
				for _, credential := range derivation.Credentials {
					values.unverified[credential] = true
				}
				continue
			}
			for credential, plaintext := range derivation.Plaintexts(state.document[property]) {
				if existing, seen := values.plaintexts[credential]; seen && existing != plaintext {
					return credentialValues{}, fmt.Errorf("%s is held with two different values, in %s and %s: the store no longer federates; resolve it before planning",
						credential, heldIn[credential], state.secret.RemoteKey+"#"+property)
				}
				values.plaintexts[credential] = plaintext
				heldIn[credential] = state.secret.RemoteKey + "#" + property
			}
		}
	}
	for _, credential := range federation.Credentials() {
		switch {
		case values.plaintexts[credential] != "":
			delete(values.unverified, credential)
			plan.Credentials = append(plan.Credentials, CredentialPlan{Credential: credential, Action: ActionKeep, Source: "held in " + heldIn[credential]})
		case values.unverified[credential]:
			plan.Credentials = append(plan.Credentials, CredentialPlan{Credential: credential, Action: ActionUnverified, Source: "may be held in a key that was not read; minted if not"})
		default:
			values.plaintexts[credential] = solutionrun.MintCredential()
			plan.Credentials = append(plan.Credentials, CredentialPlan{Credential: credential, Action: ActionGenerate, Source: "minted: no key holds it"})
		}
	}
	return values, nil
}

// planDerived plans a federation-derived property, returning the value to write
// when it changes.
func planDerived(state *remoteState, property string, derivation solutionrun.SecretDerivation, credentials credentialValues) (PropertyPlan, string) {
	names := make([]string, 0, len(derivation.Credentials))
	inputsUnverified := false
	for _, credential := range derivation.Credentials {
		names = append(names, credential.String())
		inputsUnverified = inputsUnverified || credentials.unverified[credential]
	}
	source := "derived from " + strings.Join(names, ", ")
	if derivation.Digest {
		source = "digests of " + strings.Join(names, ", ")
	}
	if !state.known() {
		return PropertyPlan{Property: property, Action: ActionUnverified, Source: source + " if absent or stale"}, ""
	}
	existing, present := state.document[property]
	if inputsUnverified {
		action := ActionDerive
		if present {
			action = ActionUnverified
		}
		return PropertyPlan{Property: property, Action: action, Source: source + " (inputs not read)"}, ""
	}
	value, err := derivation.Value(credentials.plaintexts)
	if err != nil {
		return PropertyPlan{Property: property, Action: ActionRequire, Source: source + ": " + err.Error()}, ""
	}
	switch {
	case present && existing == value:
		return PropertyPlan{Property: property, Action: ActionKeep, Source: source}, ""
	case present:
		return PropertyPlan{Property: property, Action: ActionUpdate, Source: source + " (stored value no longer encodes them)"}, value
	default:
		return PropertyPlan{Property: property, Action: ActionDerive, Source: source}, value
	}
}

// configurationKey reports whether a stored key is a configuration value —
// workspace- or service-scoped — which is one value wherever it is read. Any
// other key belongs to the remote key reading it alone.
func configurationKey(key string) bool {
	return strings.HasPrefix(key, resources.WorkspaceSecretConfigurationPrefix+"__") ||
		strings.HasPrefix(key, resources.ServiceSecretConfigurationPrefix+"__")
}

// planConfigured plans a property that is not federation-derived.
func planConfigured(state *remoteState, property string, states []*remoteState, generators map[string]environments.EnvironmentSecretGenerator, generated map[string]string) (PropertyPlan, string, error) {
	if state.known() {
		if _, present := state.document[property]; present {
			return PropertyPlan{Property: property, Action: ActionKeep, Source: "stored"}, "", nil
		}
	}
	var holders, unread []string
	var holderValue string
	if configurationKey(property) {
		for _, other := range states {
			if other == state || !slices.Contains(other.secret.Properties, property) {
				continue
			}
			if !other.known() {
				unread = append(unread, other.secret.RemoteKey)
				continue
			}
			value, present := other.document[property]
			if !present {
				continue
			}
			if len(holders) > 0 && value != holderValue {
				return PropertyPlan{}, "", fmt.Errorf("%s holds different values in %s and %s: one configuration value cannot be propagated from two", property, holders[0], other.secret.RemoteKey)
			}
			holders = append(holders, other.secret.RemoteKey)
			holderValue = value
		}
	}
	fallback, value, err := fallbackSource(property, generators, generated)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	switch {
	case !state.known():
		source := fallback.Source
		if len(holders) > 0 {
			source = "propagated from " + strings.Join(holders, ", ")
		}
		return PropertyPlan{Property: property, Action: ActionUnverified, Source: "if absent: " + source}, "", nil
	case len(holders) > 0:
		return PropertyPlan{Property: property, Action: ActionPropagate, Source: "from " + strings.Join(holders, ", ")}, holderValue, nil
	case len(unread) > 0:
		return PropertyPlan{Property: property, Action: ActionUnverified, Source: fmt.Sprintf("may propagate from %s (not read); else %s", strings.Join(unread, ", "), fallback.Source)}, "", nil
	default:
		return fallback, value, nil
	}
}

// fallbackSource is what produces a property no remote key holds: a declared
// generator, or the operator.
func fallbackSource(property string, generators map[string]environments.EnvironmentSecretGenerator, generated map[string]string) (PropertyPlan, string, error) {
	generator, declared := generators[property]
	if !declared {
		return PropertyPlan{Property: property, Action: ActionRequire, Source: "no key holds it, no federation derivation covers it, and service-secrets.generate declares no generator for it"}, "", nil
	}
	format, size := generatorFormat(&generator)
	value, ok := generated[property]
	if !ok {
		var err error
		value, err = secretgen.Generate(format, size)
		if err != nil {
			return PropertyPlan{}, "", err
		}
		generated[property] = value
	}
	return PropertyPlan{Property: property, Action: ActionGenerate, Source: fmt.Sprintf("service-secrets.generate %s/%s (%s, %d bytes)", generator.Scope, generator.Configuration, format, size)}, value, nil
}

func generatorFormat(generator *environments.EnvironmentSecretGenerator) (secretgen.Format, int) {
	format := secretgen.Format(generator.Format)
	if format == "" {
		format = secretgen.FormatHex
	}
	size := generator.Bytes
	if size == 0 {
		size = 32
		if format == secretgen.FormatIdentifier {
			size = 12
		}
	}
	return format, size
}

// generatorIndex maps every stored key a generator covers to it. A key two
// generators cover is refused: which format wins would be an accident.
func generatorIndex(generators []environments.EnvironmentSecretGenerator, services []string) (map[string]environments.EnvironmentSecretGenerator, error) {
	index := map[string]environments.EnvironmentSecretGenerator{}
	for i := range generators {
		generator := generators[i]
		for _, key := range generator.StoredKeys(services) {
			if _, duplicate := index[key]; duplicate {
				return nil, fmt.Errorf("service-secrets.generate covers %s twice", key)
			}
			index[key] = generator
		}
	}
	return index, nil
}

// Apply writes the plan's changes. It refuses a plan that did not read the store
// — it would overwrite properties it never saw — and, unless allowMissing, one
// that leaves a property the operator must supply. Writes carrying plaintexts go
// before the registrar's digests that admit them. It returns the keys written,
// which on error are the ones written before it.
func (plan *Plan) Apply(ctx context.Context, store Store, allowMissing bool) ([]string, error) {
	if plan.Unverified() {
		return nil, fmt.Errorf("the plan did not read every existing remote key, so it cannot be applied without overwriting what it never saw")
	}
	if required := plan.Required(); len(required) > 0 && !allowMissing {
		return nil, fmt.Errorf("%d properties have no source and must be supplied first (or pass --allow-missing to write the rest): %s", len(required), strings.Join(required, ", "))
	}
	var written []string
	for _, write := range plan.writes {
		if err := store.Write(ctx, write.key, write.document, write.create); err != nil {
			return written, fmt.Errorf("write %s: %w", write.key, err)
		}
		written = append(written, write.key)
	}
	return written, nil
}
