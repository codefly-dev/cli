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
//  2. propagate — a configuration value is one value however many services read
//     it, so a key another remote secret already holds is copied from there;
//  3. generate — a key the environment's `service-secrets.generate` declares
//     random;
//  4. require — anything else is supplied from outside, and named.
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
}

type pendingWrite struct {
	key      string
	document map[string]string
	create   bool
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
}

// remoteState is one remote key as the plan sees it.
type remoteState struct {
	secret      gitops.RenderedServiceSecret
	description Description
	// backend identifies the store this key was read from, as Store.Name()
	// reports it. A render that reads through several backends resolves every
	// property against all of their states at once — one configuration value is
	// one value wherever it is held — while propagating and writing only within
	// a backend. See pkg/deploysecrets/backends.go.
	backend string
	// document is nil when the key exists and was not read.
	document map[string]string
}

func (state *remoteState) known() bool { return state.document != nil }

// Build resolves every property the rendered environment reads to a source,
// through one store. A render whose keys read through several backends is
// resolved by BuildAll, which holds the rules that span them.
func Build(ctx context.Context, in *Inputs) (*Plan, error) {
	if err := validateRenderedProperties(in.Rendered); err != nil {
		return nil, err
	}
	backend := in.Store.Name()
	states, err := readStates(ctx, in, in.Store, backend, in.Rendered.Secrets)
	if err != nil {
		return nil, err
	}
	generators, err := generatorIndex(in.Generators, in.Services)
	if err != nil {
		return nil, err
	}
	return planBackend(in, backend, states, states, generators, map[string]string{}, configurationAliases(states))
}

// validateRenderedProperties refuses a render that cannot be planned at all,
// before any backend is resolved or read.
func validateRenderedProperties(rendered gitops.RenderedEnvironment) error {
	for _, secret := range rendered.Secrets {
		for _, property := range secret.Properties {
			if property.Property == "" {
				return fmt.Errorf("remote key %s is read as a bare value; only a JSON document read by property can be planned", secret.RemoteKey)
			}
			for _, key := range property.Keys {
				if strings.TrimSpace(key) == "" {
					return fmt.Errorf("remote key %s property %s records an empty secret key", secret.RemoteKey, property.Property)
				}
			}
			if len(property.Keys) == 0 {
				// Without the key nothing can tell whether the property is a shared
				// configuration value or a declared generator, and both would fall
				// to `require` — the hand-typed outcome this verb exists to remove.
				// Refuse rather than read the property name as if it were the key.
				return fmt.Errorf("remote key %s property %s records no secret key: re-render the environment so the plan knows what that value is",
					secret.RemoteKey, property.Property)
			}
		}
	}
	return nil
}

// planBackend resolves one backend's share of a render.
//
// local are the states of this backend; all are the states of every backend the
// run read, which is what the one-value rules are judged against. generated is
// shared by every backend, so a value this run mints for a stored key is minted
// once and written wherever that key is read.
func planBackend(in *Inputs, backend string, local, all []*remoteState,
	generators map[string]environments.EnvironmentSecretGenerator, generated map[string]string,
	aliases map[propertyLocation][]string) (*Plan, error) {
	plan := &Plan{Store: backend, Modules: in.Modules}
	for _, state := range local {
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
			propertyPlan, value, err := planConfigured(state, property, all, generators, generated, aliases[propertyLocation{state.backend, state.secret.RemoteKey, property.Property}])
			if err != nil {
				return nil, err
			}
			if value != "" {
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
			plan.writes = append(plan.writes, pendingWrite{key: state.secret.RemoteKey, document: document, create: !state.description.Exists})
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
func readStates(ctx context.Context, in *Inputs, store Store, backend string,
	secrets []gitops.RenderedServiceSecret) ([]*remoteState, error) {
	states := make([]*remoteState, len(secrets))
	errs := make([]error, len(secrets))
	// The first failure cancels the rest: there is no plan without every key, and
	// a store that refuses one command usually refuses the next hundred too.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wait sync.WaitGroup
	slots := make(chan struct{}, storeReaders)
	for i := range secrets {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if ctx.Err() != nil {
				return
			}
			secret := secrets[i]
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
			description, err := store.Describe(ctx, secret.RemoteKey)
			if fail(wrapKeyError("describe", secret.RemoteKey, err)) {
				return
			}
			state := &remoteState{secret: secret, description: description, backend: backend}
			switch {
			case !description.Exists || !description.HasVersion:
				state.document = map[string]string{}
			case in.ReadPayloads:
				document, err := store.Read(ctx, secret.RemoteKey)
				if fail(wrapKeyError("read", secret.RemoteKey, err)) {
					return
				}
				state.document = document
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
// states spans every backend this run read, and the two roles a holder can play
// are deliberately different (see pkg/deploysecrets/backends.go):
//
//   - a holder in THIS backend is compared with and propagated from. One value
//     is one value, and a key its own backend already holds elsewhere is the one
//     place it was seeded.
//   - a holder in ANOTHER backend is compared with and never propagated from.
//     Comparing is what makes a divergence between backends observable at all;
//     propagating would move a value between backends, which a per-key store
//     declaration exists to prevent. A property only another backend holds is
//     therefore REQUIRED here, naming where the value already is — not minted,
//     because minting beside an existing value is exactly the divergence.
func planConfigured(state *remoteState, property gitops.RenderedSecretProperty, states []*remoteState,
	generators map[string]environments.EnvironmentSecretGenerator, generated map[string]string, keys []string) (PropertyPlan, string, error) {
	lookup := property
	lookup.Keys = keys
	if _, _, err := declaredGenerator(lookup, generators); err != nil {
		return PropertyPlan{}, "", err
	}
	held, err := gatherConfigurationHolders(state, property, lookup, states)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	if err = held.agree(state, property); err != nil {
		return PropertyPlan{}, "", err
	}
	if _, present := state.stored(property.Property); present {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionKeep, Source: "stored"}, "", nil
	}
	fallback, value, err := fallbackSource(lookup, generators, generated)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	fallback.Keys = property.Keys
	switch {
	case !state.known():
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionUnverified,
			Source: "if absent: " + held.ifAbsent(fallback.Source)}, "", nil
	case len(held.local) > 0:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionPropagate,
			Source: "from " + strings.Join(held.local, ", ")}, held.value, nil
	case len(held.foreign) > 0:
		// Neither minted nor copied: see the contract above. The operator is told
		// which backend already holds the value so the copy is theirs to make.
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire,
			Source: held.elsewhere()}, "", nil
	case len(held.unread) > 0:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionUnverified,
			Source: fmt.Sprintf("may propagate from %s (not read); else %s", strings.Join(held.unread, ", "), fallback.Source)}, "", nil
	default:
		return fallback, value, nil
	}
}

// stored is what this key holds for a property, when the key was read at all.
func (state *remoteState) stored(property string) (string, bool) {
	if !state.known() {
		return "", false
	}
	value, present := state.document[property]
	return value, present
}

// configurationHolders is what every other key of a run holds for one
// property's configuration value: the locations in THIS backend, which may be
// propagated from, and the locations in other backends, which may only be
// compared with. See BuildAll for why the two differ.
type configurationHolders struct {
	local []string
	value string

	foreign      []string
	foreignValue string

	// unread are locations in this backend that exist and were not read, so
	// whether they hold the value is unknown.
	unread []string
}

// gatherConfigurationHolders collects every location holding the same
// configuration value as this property, refusing two locations of one backend
// that disagree as it goes.
func gatherConfigurationHolders(state *remoteState, property, lookup gitops.RenderedSecretProperty,
	states []*remoteState) (*configurationHolders, error) {
	held := &configurationHolders{}
	for _, other := range states {
		for _, candidate := range other.secret.Properties {
			if (other != state || candidate.Property != property.Property) && !sharesConfigurationKey(lookup, candidate) {
				continue
			}
			location := other.secret.RemoteKey + "#" + candidate.Property
			if other.backend != state.backend {
				// Another backend: compared, never propagated from.
				value, present := other.stored(candidate.Property)
				if !present {
					continue
				}
				elsewhere := other.backend + " " + location
				if len(held.foreign) > 0 && value != held.foreignValue {
					return held, divergedConfigurationValue(held.foreign[0], elsewhere)
				}
				held.foreign = append(held.foreign, elsewhere)
				held.foreignValue = value
				continue
			}
			if !other.known() {
				held.unread = append(held.unread, location)
				continue
			}
			value, present := other.stored(candidate.Property)
			if !present {
				continue
			}
			if len(held.local) > 0 && value != held.value {
				return held, divergedConfigurationValue(held.local[0], location)
			}
			held.local = append(held.local, location)
			held.value = value
		}
	}
	return held, nil
}

// agree refuses what the holders and this key's own stored value say when they
// cannot all be one value — across backends as much as within one, because a
// divergence no run reports is one no operator can act on.
func (held *configurationHolders) agree(state *remoteState, property gitops.RenderedSecretProperty) error {
	if len(held.local) > 0 && len(held.foreign) > 0 && held.value != held.foreignValue {
		return divergedConfigurationValue(held.local[0], held.foreign[0])
	}
	stored, present := state.stored(property.Property)
	if !present {
		return nil
	}
	if len(held.local) > 0 && stored != held.value {
		return fmt.Errorf("%s holds different values in %s and %s: one configuration value cannot be two; resolve it before planning",
			strings.Join(property.Keys, ", "), state.secret.RemoteKey, held.local[0])
	}
	if len(held.foreign) > 0 && stored != held.foreignValue {
		return fmt.Errorf("%s holds different values in %s %s and %s: one configuration value cannot be two; resolve it before planning",
			strings.Join(property.Keys, ", "), state.backend, state.secret.RemoteKey, held.foreign[0])
	}
	return nil
}

// ifAbsent is what would produce this property if the unread key turns out not
// to hold it.
func (held *configurationHolders) ifAbsent(fallback string) string {
	switch {
	case len(held.local) > 0:
		return "propagated from " + strings.Join(held.local, ", ")
	case len(held.foreign) > 0:
		return held.elsewhere()
	default:
		return fallback
	}
}

// elsewhere names where a configuration value already lives when it lives in a
// backend this plan does not write.
func (held *configurationHolders) elsewhere() string {
	return "held in another backend (" + strings.Join(held.foreign, ", ") +
		"): `codefly deploy secrets` never moves a value between backends, so supply it here"
}

func divergedConfigurationValue(one, other string) error {
	return fmt.Errorf("one configuration value cannot be two: different values in %s and %s; resolve it before planning", one, other)
}

// fallbackSource is what produces a property no remote key holds: a declared
// generator, or the operator.
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
	generated map[string]string) (PropertyPlan, string, error) {
	declaredKey, generator, err := declaredGenerator(property, generators)
	if err != nil {
		return PropertyPlan{}, "", err
	}
	if declaredKey == "" {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire,
			Source: "no key holds it, and service-secrets.generate declares no generator for " +
				strings.Join(property.Keys, ", ")}, "", nil
	}
	format, size := generatorFormat(&generator)
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

// Apply writes the plan's changes, in the render's order. It refuses a plan
// that did not read the store — it would overwrite properties it never saw —
// and, unless allowMissing, one that leaves a property the operator must
// supply. It returns the keys written, which on error are the ones written
// before it.
func (plan *Plan) Apply(ctx context.Context, store Store, allowMissing bool) ([]string, error) {
	if err := plan.ValidateApply(allowMissing); err != nil {
		return nil, err
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
		candidateFormat, candidateSize := generatorFormat(&candidate)
		format, size := generatorFormat(&generator)
		if declaredKey != "" && (candidateFormat != format || candidateSize != size) {
			return "", environments.EnvironmentSecretGenerator{}, fmt.Errorf("property %s is read as both %s and %s, which service-secrets.generate declares differently",
				property.Property, declaredKey, key)
		}
		declaredKey, generator = key, candidate
	}
	return declaredKey, generator, nil
}
