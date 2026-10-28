// Package deploysecrets derives what an environment's secret store must hold for
// a rendered environment to materialize, and writes it: `codefly deploy
// secrets`.
//
// A render never carries a secret value. It names, in each service's projected
// ExternalSecret, the remote key and property every secret it reads resolves
// from, and leaves the store to hold them. This package reads those names back
// from the render and resolves each one to a source, in order:
//
//  1. keep — the store already holds it; a stored value is never rotated
//     unless the operator names its generated key with --rotate (rotate.go);
//  2. propagate — a configuration value is one value however many services read
//     it, so a key another remote secret already holds is copied from there;
//  3. supply — a key the operator handed this run, by secret key, because no
//     key holds it and nothing can derive it;
//  3. configured — the environment's own configuration holds the value, in the
//     `configurations/<profile>/<group>.secret.env` that `codefly config set`
//     writes (see configured.go), so the operator's existing file seeds the
//     store instead of being retyped into it;
//  4. generate — a key the environment's `service-secrets.generate` declares
//     random;
//  5. require — anything else is supplied from outside, and named.
//
// Every one of those resolutions is named by the SECRET KEY the render reads —
// the CODEFLY__… name core gives a configuration value — never by the property
// the environment files that key under. The two are the same string only when an
// environment happens to file a key under its own name; one that maps keys to
// human-named properties (`property: postgres_user`) makes them differ for every
// key it maps, and resolving by the property then matches no shared
// configuration value and no declared generator. Each of those is a value the
// system derives, reported instead as one to type by hand.
//
// Values never leave the package: a plan reports property names, the keys read
// from them and sources only, and the writes it carries are unexported.
package deploysecrets

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/secretgen"
	"github.com/codefly-dev/core/resources"
)

// Action is what a plan does for one stored property.
type Action string

const (
	// ActionKeep: the store holds the property, and it is left as it is.
	ActionKeep Action = "keep"
	// ActionPropagate: the property is absent here and copied from the remote key
	// that already holds the same configuration value.
	ActionPropagate Action = "propagate"
	// ActionConfigured: the property is absent and the environment's own
	// configuration holds the value, in the file the Source names.
	ActionConfigured Action = "configured"
	// ActionGenerate: the property is absent and generated at random, as the
	// environment declares.
	ActionGenerate Action = "generate"
	// ActionSupply: the property is absent and written from the value the
	// operator supplied for its secret key.
	ActionSupply Action = "supply"
	// ActionRequire: the property is absent and nothing can produce it; the
	// operator supplies it.
	ActionRequire Action = "require"
	// ActionEmpty: the property is absent, nothing supplies it, and the
	// environment declares in service-secrets.may-be-empty that its correct value
	// here is empty; it is stored empty so the workload's secret materializes.
	ActionEmpty Action = "empty"
	// ActionUnverified: the remote key exists but was not read, so whether it
	// holds the property is unknown. Source says what would happen if it does not.
	ActionUnverified Action = "unverified"
	// ActionRotate: the property is stored and replaced, because --rotate names
	// its generated key or because its stored value is derived from one that is.
	ActionRotate Action = "rotate"
)

// PropertyPlan is the plan for one property of one remote key. It carries no
// value.
type PropertyPlan struct {
	Property string
	// Keys are the secret keys the render reads out of this property — what the
	// value is, as against where it is filed. They are reported beside the
	// property because an operator asked to supply a value needs the key to know
	// what to put there: "lodestar-store#postgres_user" says where, and only the
	// key says it is a database owner name.
	Keys   []string
	Action Action
	Source string
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

// Plan is a resolved environment. Its exported fields name keys and sources
// only; the values it would write stay inside it.
type Plan struct {
	Store string
	// Modules is the scope the plan was limited to; empty plans every rendered
	// module.
	Modules []string
	Secrets []SecretPlan

	writes []pendingWrite
	states []*remoteState
	// rotating makes Apply all-or-nothing: a rotation half-written is a source
	// and its derived values disagreeing, so a failed write restores the rest.
	rotating bool
}

type pendingWrite struct {
	key      string
	document map[string]string
	create   bool
	// changed names the properties this write sets; previous is the document it
	// replaces (empty for a key it creates), which a failed rotation restores.
	changed  []string
	previous map[string]string
	// version is the newest version the plan read; the write is refused when
	// the key no longer has it.
	version string
}

// Written is one remote key an apply wrote: the properties it set and the
// version the store assigned. Restored marks the write of the previous document
// after a rotation failed part-way. It carries no value.
type Written struct {
	Key        string
	Properties []string
	Version    string
	Restored   bool
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

// Required lists, as `remote-key#property (key)`, every property the operator
// must supply. The secret key rides along: the property alone says where the
// value is filed, not what it is.
func (plan *Plan) Required() []string {
	var required []string
	for _, secret := range plan.Secrets {
		for _, property := range secret.Properties {
			if property.Action == ActionRequire {
				required = append(required, secret.RemoteKey+"#"+property.Property+" ("+strings.Join(property.Keys, ", ")+")")
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
	Generators []environments.EnvironmentSecretGenerator
	// Services are the workspace's service uniques, which a service-scoped
	// generator naming no services covers.
	Services []string
	Store    Store
	// ReadPayloads reads every existing remote key, in memory, to know which
	// properties it holds. Without it the plan is metadata only: it knows which
	// keys exist, not what they hold, and cannot be applied.
	ReadPayloads bool
	// Modules limits the plan to the remote keys the services of these modules
	// read. Every other key is still described and read — the configuration
	// values it holds are what a scoped key keeps agreeing with — but nothing
	// else is planned or written. Empty plans the whole environment.
	Modules []string
	// MayBeEmpty are the secret keys this environment declares as legitimately
	// empty, from service-secrets.may-be-empty. A present-but-empty property is
	// otherwise refused.
	MayBeEmpty []string
	// MintKeys explicitly acknowledges first creation of lifelong signing keys.
	MintKeys []string
	// Supplied are operator values for keys no remote key holds and no generator
	// declares, by secret key. A supplied value never replaces a stored one and
	// never stands in for a declared generator. ValidateSupplied checks them
	// against the whole render first.
	Supplied map[string]string
	// Configured are the values the environment's own configuration files hold,
	// by the secret key a render reads each one under — ReadConfigured. A
	// property no remote key holds is resolved from here before a generator is
	// considered, and reported as ActionConfigured naming the file. Nil plans
	// without the operator's files, exactly as before they were read.
	Configured map[string]ConfiguredValue
	// Rotate are generated secret keys (RotationKey) whose stored value is
	// replaced with a newly generated one, together with every stored value
	// derived from it. A key no generator declares is refused.
	Rotate []string
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
	plan := &Plan{Store: in.Store.Name(), Modules: in.Modules}

	for _, secret := range in.Rendered.Secrets {
		for _, property := range secret.Properties {
			if property.Property == "" {
				return nil, fmt.Errorf("remote key %s is read as a bare value; only a JSON document read by property can be planned", secret.RemoteKey)
			}
			for _, key := range property.Keys {
				if strings.TrimSpace(key) == "" {
					return nil, fmt.Errorf("remote key %s property %s records an empty secret key", secret.RemoteKey, property.Property)
				}
			}
			if len(property.Keys) == 0 {
				// Without the key nothing can tell whether the property is a shared
				// configuration value or a declared generator, and both would fall
				// to `require` — the hand-typed outcome this verb exists to remove.
				// Refuse rather than read the property name as if it were the key.
				return nil, fmt.Errorf("remote key %s property %s records no secret key: re-render the environment so the plan knows what that value is",
					secret.RemoteKey, property.Property)
			}
		}
	}
	states, err := readStates(ctx, in)
	if err != nil {
		return nil, err
	}
	plan.states = states

	generators, err := generatorIndex(in.Generators, in.Services)
	if err != nil {
		return nil, err
	}
	aliases := configurationAliases(states)

	// generated holds one value per stored configuration key, so every remote key
	// reading it receives the same one.
	generated := map[string]string{}
	rotating, derived, err := planRotations(in, states, generators, generated, aliases)
	if err != nil {
		return nil, err
	}
	plan.rotating = len(rotating) > 0
	for _, state := range states {
		if !planning(&state.secret, in.Modules) {
			continue
		}
		secretPlan := SecretPlan{
			RemoteKey:  state.secret.RemoteKey,
			Services:   state.secret.Services,
			Exists:     state.description.Exists,
			HasVersion: state.description.HasVersion,
			Read:       state.known(),
		}
		changes := map[string]string{}
		for _, property := range state.secret.Properties {
			location := propertyLocation{state.secret.RemoteKey, property.Property}
			propertyPlan, value, err := planConfigured(state, property, states, generators, generated, in.Configured,
				aliases[location], rotating, in.MayBeEmpty, in.MintKeys, in.Supplied)
			if err != nil {
				return nil, err
			}
			if rewrite, ok := derived[location]; ok {
				propertyPlan = PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRotate,
					Source: "derived: the stored URL's password follows rotated " + strings.Join(rewrite.from, ", ")}
				value = rewrite.value
			}
			if value != "" || propertyPlan.Action == ActionEmpty || propertyPlan.Action == ActionSupply {
				changes[property.Property] = value
			}
			secretPlan.Properties = append(secretPlan.Properties, propertyPlan)
		}
		if len(secretPlan.Properties) == 0 {
			continue
		}
		plan.Secrets = append(plan.Secrets, secretPlan)
		if len(changes) > 0 && state.known() {
			document := maps.Clone(state.document)
			maps.Copy(document, changes)
			plan.writes = append(plan.writes, pendingWrite{key: state.secret.RemoteKey, document: document, create: !state.description.Exists,
				changed: slices.Sorted(maps.Keys(changes)), previous: state.document, version: state.description.Version})
		}
	}
	return plan, nil
}

// storeReaders bounds how many backend commands run at once. Each remote key
// costs a describe, a version listing and a read, and every one of them is a
// process: a hundred-key environment is three hundred `gcloud` invocations, which
// serially is minutes of process startup and nothing else. The keys are
// independent, so they are gathered together; the bound keeps a large render
// from forking a process per key at once.
const storeReaders = 8

// readStates gathers every rendered key's description, and its document when the
// plan reads payloads. A key's own failure is recorded against its index and the
// first in the render's order is returned, so a failure names the same key
// however the work happened to be scheduled.
func readStates(ctx context.Context, in *Inputs) ([]*remoteState, error) {
	states := make([]*remoteState, len(in.Rendered.Secrets))
	errs := make([]error, len(in.Rendered.Secrets))
	// The first failure cancels the rest: there is no plan without every key, and
	// a store that refuses one command usually refuses the next hundred too.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wait sync.WaitGroup
	slots := make(chan struct{}, storeReaders)
	for i := range in.Rendered.Secrets {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if ctx.Err() != nil {
				return
			}
			secret := in.Rendered.Secrets[i]
			// fail records a failure only when it is this key's own. Once one key
			// has failed, every other key's command fails too, with the
			// cancellation — recording those would bury the one error that says
			// what actually went wrong behind whichever key sorts first.
			fail := func(err error) bool {
				if err == nil {
					return false
				}
				if ctx.Err() == nil {
					errs[i] = err
					cancel()
				}
				return true
			}
			description, err := in.Store.Describe(ctx, secret.RemoteKey)
			if fail(wrapKeyError("describe", secret.RemoteKey, err)) {
				return
			}
			state := &remoteState{secret: secret, description: description}
			if description.Exists && !description.HasVersion {
				fail(fmt.Errorf("remote key %s exists without a readable version; refusing to regenerate retained material", secret.RemoteKey))
				return
			}
			switch {
			case !description.Exists:
				state.document = map[string]string{}
			case in.ReadPayloads:
				document, err := in.Store.Read(ctx, secret.RemoteKey)
				if fail(wrapKeyError("read", secret.RemoteKey, err)) {
					return
				}
				state.document = document
				current, err := in.Store.Describe(ctx, secret.RemoteKey)
				if fail(wrapKeyError("describe after read", secret.RemoteKey, err)) {
					return
				}
				if current != description {
					fail(fmt.Errorf("remote key %s changed while reading; rebuild the plan", secret.RemoteKey))
					return
				}
			}
			// An empty stored value is refused, because an empty credential is
			// almost always a half-finished seed. Two things narrow that:
			//
			//   - the environment may DECLARE a key as legitimately empty, for
			//     a key that has no value to hold on this cell;
			//   - only a key THIS run plans is checked. A scoped run reads every
			//     remote key so a scoped key keeps agreeing with the store, but
			//     an out-of-scope key it will never write is not its business to
			//     refuse -- otherwise one unrelated service's optional key makes
			//     `--module` unusable.
			for _, property := range secret.Properties {
				value, present := state.document[property.Property]
				if !present || strings.TrimSpace(value) != "" {
					continue
				}
				if declaredEmpty(property, in.MayBeEmpty) {
					continue
				}
				if !planning(&secret, in.Modules) {
					continue
				}
				fail(fmt.Errorf("remote key %s property %s is empty: supply a nonempty value, or declare it in service-secrets.may-be-empty if this environment holds no value for it", secret.RemoteKey, property.Property))
				return
			}
			states[i] = state
		}(i)
	}
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	for i := range states {
		if states[i] == nil {
			// Nothing recorded a failure, so the caller's own context ended.
			return nil, ctx.Err()
		}
	}
	return states, nil
}

// declaredEmpty reports whether every secret key this property is read under is
// declared legitimately empty. Every key, not any: a property two services read
// under two keys is only safely empty when neither reader needs a value.
func declaredEmpty(property gitops.RenderedSecretProperty, mayBeEmpty []string) bool {
	if len(property.Keys) == 0 || len(mayBeEmpty) == 0 {
		return false
	}
	for _, key := range property.Keys {
		if !slices.Contains(mayBeEmpty, key) {
			return false
		}
	}
	return true
}

// planning reports whether this remote key is one the run would write. With no
// --module every rendered key is in scope; with one, only the keys a listed
// module's services read. Build plans on this test and readStates refuses an
// empty value on it, deliberately the same one on the same module/service
// uniques, so what a run refuses and what it writes cannot drift apart.
func planning(secret *gitops.RenderedServiceSecret, modules []string) bool {
	if len(modules) == 0 {
		return true
	}
	for _, unique := range secret.Services {
		module, _, _ := strings.Cut(unique, "/")
		if slices.Contains(modules, module) {
			return true
		}
	}
	return false
}

// wrapKeyError names the key and the command that failed on it, and passes nil
// through so a caller can test the result rather than the input.
func wrapKeyError(verb, key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", verb, key, err)
}

// configurationKey reports whether a stored key is a configuration value —
// workspace- or service-scoped — which is one value wherever it is read. Any
// other key belongs to the remote key reading it alone.
func configurationKey(key string) bool {
	return strings.HasPrefix(key, resources.WorkspaceSecretConfigurationPrefix+"__") ||
		strings.HasPrefix(key, resources.ServiceSecretConfigurationPrefix+"__")
}

// sharesConfigurationKey reports whether two properties carry the same
// configuration value: some configuration key is read out of both.
//
// Two remote keys hold one configuration value when the SAME secret key is read
// from each, whatever each environment filed it under — `lodestar-accounts` may
// file it as `internal_token` and another entry under the key's own name, and it
// is still one value. Matching the property names instead both misses that pair
// and, worse, pairs two unrelated values two remote keys happen to file under
// one word.
func sharesConfigurationKey(a, b gitops.RenderedSecretProperty) bool {
	for _, key := range a.Keys {
		if configurationKey(key) && slices.Contains(b.Keys, key) {
			return true
		}
	}
	return false
}

// refuseSharedSigningKey refuses an ed25519-pkcs8 value that more than one
// remote key reads. A signing key is one workload's identity: filed in two
// services' documents, either could sign as the other. Checked here, where the
// readers are known, because a workspace group's readers are not known when
// the environment loads.
func refuseSharedSigningKey(state *remoteState, property gitops.RenderedSecretProperty, lookup gitops.RenderedSecretProperty,
	states []*remoteState, declaredKey string) error {
	// Counted by SERVICE, not by remote key: one remote key can be read by several
	// services (a defaults template without {service}, or a remote-keys mapping),
	// and each of them would be handed the same private key.
	readers := map[string]struct{}{}
	for _, other := range states {
		for _, candidate := range other.secret.Properties {
			if (other != state || candidate.Property != property.Property) && !sharesConfigurationKey(lookup, candidate) {
				continue
			}
			if len(candidate.Readers) == 0 {
				for _, service := range other.secret.Services {
					readers[service] = struct{}{}
				}
				if len(other.secret.Services) == 0 {
					readers[other.secret.RemoteKey] = struct{}{}
				}
				continue
			}
			for _, reader := range candidate.Readers {
				readers[reader.Service] = struct{}{}
			}
		}
	}
	if len(readers) > 1 {
		names := make([]string, 0, len(readers))
		for name := range readers {
			names = append(names, name)
		}
		slices.Sort(names)
		return fmt.Errorf("%s is an ed25519-pkcs8 signing key read by %d services (%s): a signing key is one workload's, so exactly one may read it — remove the other readers; never re-mint it",
			declaredKey, len(readers), strings.Join(names, ", "))
	}
	return nil
}

// planConfigured plans one property.
//
// The other keys holding the same configuration value are looked at before this
// key's own state, not only when this key lacks it. A configuration value is one
// value however many services read it, so two keys holding it with different
// values is a store that no longer agrees with itself — and that is true whether
// or not a third key needs it propagated. Keeping what each key happens to hold
// would report the divergence as `keep`, which is the one reading an operator
// cannot act on.
//
// A value the environment's own configuration holds is a holder of that same
// configuration value, so the agreement rule covers it too: a stored or
// propagated value that differs from the configured one is refused by name
// rather than kept. What is NOT done is rotate anything — a store that already
// holds the property keeps it, as it always has, and the configured file is
// only consulted for a property nothing holds.
func planConfigured(state *remoteState, property gitops.RenderedSecretProperty, states []*remoteState,
	generators map[string]environments.EnvironmentSecretGenerator, generated map[string]string,
	configured map[string]ConfiguredValue, keys []string, rotating map[string]bool, mayBeEmpty, mintKeys []string, supplied map[string]string) (PropertyPlan, string, error) {
	lookup := property
	lookup.Keys = keys
	declaredKey, generator, err := declaredGenerator(lookup, generators)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	suppliedValue, isSupplied, err := suppliedFor(lookup, supplied, declaredKey)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	configuredValue, hasConfigured, err := configuredFor(lookup, configured)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	if declaredKey != "" && generator.Format == environments.SecretGeneratorFormatEd25519PKCS8 {
		if err = refuseSharedSigningKey(state, property, lookup, states, declaredKey); err != nil {
			return PropertyPlan{}, "", err
		}
	}
	holders, unread, holderValue, err := configurationHolders(state, property, lookup, states)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	if len(holders) > 0 {
		if err = refuseSuppliedConflict(property, isSupplied, suppliedValue, holders[0], holderValue); err != nil {
			return PropertyPlan{}, "", err
		}
	}
	// A configured value disagreeing with what the store holds is refused, on
	// the rule that already covers two remote keys. `holders` includes this
	// key's own stored property, so this is the stored-value case too — and it
	// has to be refused rather than kept, because a stored value is never
	// rotated: keeping it would leave the deployed workload on one value and
	// every local run reading the same file on the other, reported as `keep`,
	// the one reading that tells an operator nothing is wrong.
	if hasConfigured && len(holders) > 0 && holderValue != configuredValue.value {
		return PropertyPlan{}, "", fmt.Errorf("%s holds different values in %s and %s: one configuration value cannot be two, and a stored value is never rotated — change the configuration to match what is stored, or remove the stored property so the plan seeds it from the file",
			strings.Join(property.Keys, ", "), strings.Join(holders, ", "), configuredValue.File)
	}
	if declaredKey != "" && rotating[declaredKey] {
		if hasConfigured {
			return PropertyPlan{}, "", fmt.Errorf("--rotate %s: the value is configured in %s, which the store must keep agreeing with; change the file instead", declaredKey, configuredValue.File)
		}
		return rotatedProperty(state, property, declaredKey, generated[declaredKey])
	}
	if state.known() {
		if _, present := state.document[property.Property]; present {
			return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionKeep, Source: "stored"}, "", nil
		}
	}
	if generator.Format == environments.SecretGeneratorFormatEd25519PKCS8 && len(holders) == 0 && len(unread) == 0 {
		if state.description.Exists {
			return PropertyPlan{}, "", fmt.Errorf("remote key %s exists but lacks signing key %s; restore its original material, never re-mint it", state.secret.RemoteKey, declaredKey)
		}
		if !slices.Contains(mintKeys, declaredKey) {
			return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire,
				Source: "first signing-key creation requires --mint " + declaredKey}, "", nil
		}
	}
	fallback, value, err := fallbackSource(lookup, generators, generated, configuredValue, hasConfigured, mayBeEmpty)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	fallback.Keys = property.Keys
	switch {
	case !state.known():
		source := fallback.Source
		if len(holders) > 0 {
			source = "propagated from " + strings.Join(holders, ", ")
		}
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionUnverified, Source: "if absent: " + source}, "", nil
	case len(holders) > 0:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionPropagate, Source: "from " + strings.Join(holders, ", ")}, holderValue, nil
	case len(unread) > 0:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionUnverified,
			Source: fmt.Sprintf("may propagate from %s (not read); else %s", strings.Join(unread, ", "), fallback.Source)}, "", nil
	case isSupplied:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionSupply, Source: "supplied by the operator"}, suppliedValue, nil
	default:
		return fallback, value, nil
	}
}

// configurationHolders lists every location holding this property's
// configuration value — this key's own property and any other property read
// under one of its keys — and the one value they agree on. Unread keys are
// listed apart; two read holders that disagree are refused.
func configurationHolders(state *remoteState, property, lookup gitops.RenderedSecretProperty, states []*remoteState) (holders, unread []string, holderValue string, err error) {
	for _, other := range states {
		for _, candidate := range other.secret.Properties {
			if (other != state || candidate.Property != property.Property) && !sharesConfigurationKey(lookup, candidate) {
				continue
			}
			location := other.secret.RemoteKey + "#" + candidate.Property
			if !other.known() {
				unread = append(unread, location)
				continue
			}
			value, present := other.document[candidate.Property]
			if !present {
				continue
			}
			if len(holders) > 0 && value != holderValue {
				return nil, nil, "", fmt.Errorf("one configuration value cannot be two: different values in %s and %s; resolve it before planning", holders[0], location)
			}
			holders = append(holders, location)
			holderValue = value
		}
	}
	return holders, unread, holderValue, nil
}

// suppliedFor is the operator's value for a property, looked up by every secret
// key it is read under. Two keys of one configuration value supplied twice must
// agree, as two stored copies must, and a declared generator is never overridden.
func suppliedFor(property gitops.RenderedSecretProperty, supplied map[string]string, declaredKey string) (string, bool, error) {
	var value, from string
	for _, key := range property.Keys {
		candidate, ok := supplied[key]
		if !ok {
			continue
		}
		if from != "" && candidate != value {
			return "", false, fmt.Errorf("one configuration value cannot be two: %s and %s are supplied differently", from, key)
		}
		value, from = candidate, key
	}
	if from != "" && declaredKey != "" {
		return "", false, fmt.Errorf("%s is both supplied and declared by service-secrets.generate; drop the supplied value", declaredKey)
	}
	return value, from != "", nil
}

// refuseSuppliedConflict refuses a supplied value that differs from one already
// held at location: a supplied value never replaces a stored one.
func refuseSuppliedConflict(property gitops.RenderedSecretProperty, isSupplied bool, supplied, location, held string) error {
	if !isSupplied || supplied == held {
		return nil
	}
	return fmt.Errorf("the value supplied for %s differs from the one %s holds; a supplied value never replaces a stored one",
		strings.Join(property.Keys, ", "), location)
}

// ValidateSupplied refuses supplied values the render cannot use: a key no
// rendered property reads (a typo would otherwise be dropped silently), a key
// that is not a configuration key, and an empty value for a key the environment
// does not declare may-be-empty.
func ValidateSupplied(rendered gitops.RenderedEnvironment, supplied map[string]string, mayBeEmpty []string) error {
	read := map[string]bool{}
	for _, secret := range rendered.Secrets {
		for _, property := range secret.Properties {
			for _, key := range property.Keys {
				read[key] = true
			}
		}
	}
	var unknown, empty []string
	for key, value := range supplied {
		switch {
		case !configurationKey(key) || !read[key]:
			unknown = append(unknown, key)
		case value == "" && !slices.Contains(mayBeEmpty, key):
			empty = append(empty, key)
		}
	}
	slices.Sort(unknown)
	slices.Sort(empty)
	if len(unknown) > 0 {
		return fmt.Errorf("supplied keys no rendered property reads: %s", strings.Join(unknown, ", "))
	}
	if len(empty) > 0 {
		return fmt.Errorf("supplied empty values for keys not declared in service-secrets.may-be-empty: %s", strings.Join(empty, ", "))
	}
	return nil
}

// fallbackSource is what produces a property no remote key holds: the
// environment's own configuration, a declared generator, or the operator.
//
// The configured value comes first. It is a value an operator chose for that
// key, and a generator mints only what nothing supplies — the same reason a
// stored value is kept rather than regenerated. A key that is both configured
// and declared random is therefore seeded from the file, and the declaration
// does nothing until the file stops holding it.
//
// A generator is declared over the secret key, which is the only name that says
// what the value is; the property is where the environment files it. Looking a
// generator up by the property matches nothing whenever the two differ, and the
// declared generator then reports as "no generator declares it" — the hand-typed
// outcome, with a declaration sitting in the environment that says otherwise.
//
// A property read as two keys covered by two different generators has no answer:
// whichever won would be an accident of ordering, so it is refused.
func fallbackSource(property gitops.RenderedSecretProperty, generators map[string]environments.EnvironmentSecretGenerator,
	generated map[string]string, configured ConfiguredValue, hasConfigured bool, mayBeEmpty []string) (PropertyPlan, string, error) {
	if hasConfigured {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionConfigured,
			Source: "configured in " + configured.File}, configured.value, nil
	}
	declaredKey, generator, err := declaredGenerator(property, generators)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	if declaredKey == "" && declaredEmpty(property, mayBeEmpty) {
		// The declaration is the value: an environment that says a key is
		// legitimately empty here has said what to store. Requiring it instead
		// leaves the property absent, and the ExternalSecret reading it never
		// materializes its Secret.
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionEmpty,
			Source: "service-secrets.may-be-empty declares " + strings.Join(property.Keys, ", ") + " empty here"}, "", nil
	}
	if declaredKey == "" {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire,
			Source: "no key holds it, no configuration file supplies it, and service-secrets.generate declares no generator for " +
				strings.Join(property.Keys, ", ")}, "", nil
	}
	format, size := generator.Encoding()
	// One value per stored key, so every remote key reading that key is handed
	// the same one: a configuration value is one value however many properties
	// file it.
	value, ok := generated[declaredKey]
	if !ok {
		var err error
		value, err = secretgen.Generate(format, size)
		if err != nil {
			return PropertyPlan{}, "", err
		}
		generated[declaredKey] = value
	}
	return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionGenerate,
		Source: fmt.Sprintf("service-secrets.generate %s/%s for %s (%s, %d bytes)", generator.Scope, generator.Configuration, declaredKey, format, size)}, value, nil
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

// Apply writes the plan's changes, in the render's order, each as a new
// version. It refuses a plan that did not read the store — it would overwrite
// properties it never saw — and, unless allowMissing, one that leaves a
// property the operator must supply. It returns what it wrote, which on error
// is what it wrote before it — and, for a rotation, the restores (restore).
func (plan *Plan) Apply(ctx context.Context, store Store, allowMissing bool) ([]Written, error) {
	if err := plan.ValidateApply(allowMissing); err != nil {
		return nil, err
	}
	if store.Name() != plan.Store {
		return nil, fmt.Errorf("plan belongs to a different secret store")
	}
	// Check propagation sources as well as destinations before the first write.
	for _, state := range plan.states {
		if err := unchangedState(ctx, store, state); err != nil {
			return nil, err
		}
	}
	var written []Written
	for i, write := range plan.writes {
		if err := unchanged(ctx, store, &write); err != nil {
			if plan.rotating {
				return restore(ctx, store, written, plan.writes[:i], nil, err)
			}
			return written, err
		}
		version, err := store.Write(ctx, write.key, write.document, write.create)
		if err != nil {
			err = fmt.Errorf("write %s: %w", write.key, err)
			if plan.rotating {
				// The failed write may still have landed, so it is restored too.
				return restore(ctx, store, written, plan.writes[:i], &plan.writes[i], err)
			}
			return written, err
		}
		written = append(written, Written{Key: write.key, Properties: write.changed, Version: version})
	}
	return written, nil
}

// This is an optimistic check, not an atomic compare-and-set. Neither backend's
// version creation API offers that guarantee; operators must serialize writers.
func unchangedState(ctx context.Context, store Store, state *remoteState) error {
	key := state.secret.RemoteKey
	description, err := store.Describe(ctx, key)
	if err != nil {
		return wrapKeyError("recheck", key, err)
	}
	if description != state.description {
		return fmt.Errorf("remote key %s changed since planning; rebuild the plan", key)
	}
	if description.HasVersion {
		document, err := store.Read(ctx, key)
		if err != nil {
			return wrapKeyError("recheck", key, err)
		}
		if !maps.Equal(document, state.document) {
			return fmt.Errorf("remote key %s changed since planning; rebuild the plan", key)
		}
	}
	return nil
}

// unchanged refuses a write to a key that is no longer as the plan read it: a
// document built from what it used to hold would silently undo whatever was
// written since. The store offers no conditional version add, so this narrows
// the window to one describe; it does not close it.
func unchanged(ctx context.Context, store Store, write *pendingWrite) error {
	current, err := store.Describe(ctx, write.key)
	if err != nil {
		return wrapKeyError("describe", write.key, err)
	}
	if current.Exists != !write.create || current.Version != write.version {
		return fmt.Errorf("remote key %s changed after the plan read it (version %q, now %q): re-run to plan against what it holds now",
			write.key, write.version, current.Version)
	}
	return nil
}

// ValidateApply checks completeness before confirmation, no-op success or writes.
func (plan *Plan) ValidateApply(allowMissing bool) error {
	if plan.Unverified() {
		return fmt.Errorf("the plan did not read every existing remote key, so it cannot be applied without overwriting what it never saw")
	}
	for _, secret := range plan.Secrets {
		for _, property := range secret.Properties {
			if property.Action == ActionUnverified {
				return fmt.Errorf("remote key %s property %s has unverified inputs and cannot be applied", secret.RemoteKey, property.Property)
			}
		}
	}
	if required := plan.Required(); len(required) > 0 && !allowMissing {
		return fmt.Errorf("%d properties have no source and must be supplied first (or pass --allow-missing to write the rest): %s", len(required), strings.Join(required, ", "))
	}
	return nil
}

func declaredGenerator(property gitops.RenderedSecretProperty, generators map[string]environments.EnvironmentSecretGenerator) (string, environments.EnvironmentSecretGenerator, error) {
	var declaredKey string
	var generator environments.EnvironmentSecretGenerator
	for _, key := range property.Keys {
		candidate, declared := generators[key]
		if !declared {
			continue
		}
		candidateFormat, candidateSize := candidate.Encoding()
		format, size := generator.Encoding()
		if declaredKey != "" && (candidateFormat != format || candidateSize != size) {
			return "", environments.EnvironmentSecretGenerator{}, fmt.Errorf("property %s is read as both %s and %s, which service-secrets.generate declares differently",
				property.Property, declaredKey, key)
		}
		declaredKey, generator = key, candidate
	}
	return declaredKey, generator, nil
}
