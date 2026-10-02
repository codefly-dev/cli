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
// Every one of those resolutions is named by the SECRET KEY the render reads —
// the CODEFLY__… name core gives a configuration value — never by the property
// the environment files that key under. The two are the same string only when an
// environment happens to file a key under its own name; one that maps keys to
// human-named properties (`property: postgres_user`) makes them differ for every
// key it maps, and resolving by the property then matches no federation
// derivation, no shared configuration value and no declared generator. Each of
// those is a value the system derives, reported instead as one to type by hand.
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
	Read bool
	// Counterpart names the scoped modules this key is planned for although no
	// service of theirs reads it: it is the registrar holding the digests of a
	// credential those modules present. Only those digest properties are
	// planned on it. Empty for a key in scope in its own right.
	Counterpart []string
	Properties  []PropertyPlan
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
	Store string
	// Modules is the scope the plan was limited to; empty plans every rendered
	// module.
	Modules     []string
	Secrets     []SecretPlan
	Credentials []CredentialPlan
	// Notes are the federation derivation's narration. The Warning flag is
	// carried, not flattened: it marks a line an operator has to act on — a
	// federation that cannot work — and a caller that renders it as ordinary
	// narration buries the one line that is a diagnosis.
	Notes []solutionrun.Note

	writes []pendingWrite
}

type pendingWrite struct {
	key      string
	document map[string]string
	create   bool
	// plain and digest are the credentials this write carries as a plaintext and
	// as a digest. A write is applied after every write carrying, in plaintext, a
	// credential it digests: the registrar must never admit a secret before the
	// service presenting it holds one. A single key may carry both, so the order
	// follows the credentials rather than a per-key flag.
	plain, digest []solutionrun.Credential
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
	// Modules limits the plan to the remote keys the services of these modules
	// read, plus their federation counterpart: the registrar's digest properties
	// encoding a credential those keys hold. Every other key is still described
	// and read — the credentials and configuration values it holds are what a
	// scoped key keeps agreeing with — but nothing else is planned or written.
	// Empty plans the whole environment.
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
	// document is nil when the key exists and was not read.
	document map[string]string
}

func (state *remoteState) known() bool { return state.document != nil }

// Build resolves every property the rendered environment reads to a source.
func Build(ctx context.Context, in *Inputs) (*Plan, error) {
	plan := &Plan{Store: in.Store.Name(), Modules: in.Modules}
	plan.Notes = append(plan.Notes, in.Federation.Notes...)

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
				// Without the key nothing can tell whether the property is a
				// federation credential, a shared configuration value or a declared
				// generator, and every one of them would fall to `require` — the
				// hand-typed outcome this verb exists to remove. Refuse rather than
				// read the property name as if it were the key.
				return nil, fmt.Errorf("remote key %s property %s records no secret key: re-render the environment so the plan knows what that value is",
					secret.RemoteKey, property.Property)
			}
		}
	}
	states, err := readStates(ctx, in)
	if err != nil {
		return nil, err
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
	aliases, err := configurationAliases(states, derivations)
	if err != nil {
		return nil, err
	}
	scope := newPlanScope(in.Modules, states, derivations)

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
		var carriedPlain, carriedDigest []solutionrun.Credential
		for _, property := range state.secret.Properties {
			derivation, derived := derivations[state.secret.RemoteKey][property.Property]
			inScope, counterpart := scope.property(state, derivation, derived)
			if !inScope {
				continue
			}
			var propertyPlan PropertyPlan
			var value string
			if derived {
				propertyPlan, value = planDerived(state, property, derivation, credentials)
				if value != "" {
					target := &carriedPlain
					if derivation.Digest {
						target = &carriedDigest
					}
					for _, credential := range derivation.Credentials {
						if !slices.Contains(*target, credential) {
							*target = append(*target, credential)
						}
					}
				}
				if counterpart {
					propertyPlan.Source += " (federation counterpart of " + strings.Join(in.Modules, ", ") + ")"
				}
			} else {
				propertyPlan, value, err = planConfigured(state, property, states, generators, generated, aliases[propertyLocation{state.secret.RemoteKey, property.Property}])
				if err != nil {
					return nil, err
				}
			}
			if value != "" {
				changes[property.Property] = value
			}
			secretPlan.Properties = append(secretPlan.Properties, propertyPlan)
			if value != "" && derived {
				scope.written(derivation)
			}
		}
		if len(secretPlan.Properties) == 0 {
			continue
		}
		if !scope.owns(state) {
			secretPlan.Counterpart = in.Modules
		}
		plan.Secrets = append(plan.Secrets, secretPlan)
		if len(changes) > 0 && state.known() {
			document := maps.Clone(state.document)
			maps.Copy(document, changes)
			plan.writes = append(plan.writes, pendingWrite{key: state.secret.RemoteKey, document: document,
				create: !state.description.Exists, plain: carriedPlain, digest: carriedDigest})
		}
	}
	plan.writes, err = orderWrites(plan.writes)
	if err != nil {
		return nil, err
	}
	if err := scope.check(plan); err != nil {
		return nil, err
	}
	plan.Credentials = scope.credentialPlans(plan.Credentials)
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
			switch {
			case !description.Exists || !description.HasVersion:
				state.document = map[string]string{}
			case in.ReadPayloads:
				document, err := in.Store.Read(ctx, secret.RemoteKey)
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
				if !planning(secret, in.Modules) {
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
// module's services read. Deliberately the same test planScope.owns applies,
// on the same module/service uniques, so what a run refuses and what it writes
// cannot drift apart.
func planning(secret gitops.RenderedServiceSecret, modules []string) bool {
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

// orderWrites orders the writes so that every write carrying a credential in
// plaintext precedes every write carrying a digest of that credential: a
// registrar must never admit a secret before the service presenting it holds
// one, so a run that fails part way leaves a federation that is incomplete
// rather than one that admits a secret nobody has.
//
// The order follows the credentials, not the keys. A key that carries both a
// plaintext and a digest is ordered by what it actually carries — a per-key
// "has digests" flag would push it after a key holding the plaintext it
// digests. A write carrying both the plaintext and the digest of one credential
// is one document, so it constrains nothing against itself. Two writes that
// each digest what the other holds cannot be ordered at all, and are refused
// rather than written in an order that is wrong either way.
func orderWrites(writes []pendingWrite) ([]pendingWrite, error) {
	ordered := make([]pendingWrite, 0, len(writes))
	placed := make([]bool, len(writes))
	for len(ordered) < len(writes) {
		progressed := false
		for i := range writes {
			if placed[i] || awaitsPlaintext(writes, placed, i) {
				continue
			}
			ordered = append(ordered, writes[i])
			placed[i] = true
			progressed = true
		}
		if !progressed {
			var stuck []string
			for i := range writes {
				if !placed[i] {
					stuck = append(stuck, writes[i].key)
				}
			}
			return nil, fmt.Errorf("%s each hold a credential the other digests, so no order writes a plaintext before the digest admitting it: the store cannot be seeded until one of them stops carrying both",
				strings.Join(stuck, " and "))
		}
	}
	return ordered, nil
}

// awaitsPlaintext reports whether an unplaced write still holds, in plaintext, a
// credential write i digests.
func awaitsPlaintext(writes []pendingWrite, placed []bool, i int) bool {
	for j := range writes {
		if j == i || placed[j] {
			continue
		}
		for _, credential := range writes[j].plain {
			if slices.Contains(writes[i].digest, credential) {
				return true
			}
		}
	}
	return false
}

// planScope decides which properties a module-scoped plan covers.
type planScope struct {
	modules []string
	// held are the credentials a scoped key holds in plaintext; a registrar
	// digest encoding one of them is the scope's federation counterpart.
	held []solutionrun.Credential
	// used are the credentials some planned property derives from.
	used []solutionrun.Credential
	// writtenPlain and writtenDigest are the credentials a write carries, as a
	// plaintext and as a digest.
	writtenPlain, writtenDigest []solutionrun.Credential
}

func newPlanScope(modules []string, states []*remoteState, derivations map[string]map[string]solutionrun.SecretDerivation) *planScope {
	scope := &planScope{modules: modules}
	for _, state := range states {
		if !scope.owns(state) {
			continue
		}
		for _, derivation := range derivations[state.secret.RemoteKey] {
			if derivation.Digest {
				continue
			}
			for _, credential := range derivation.Credentials {
				if !slices.Contains(scope.held, credential) {
					scope.held = append(scope.held, credential)
				}
			}
		}
	}
	return scope
}

// owns reports whether a service of a scoped module reads the key; every key is
// owned by an unscoped plan.
func (scope *planScope) owns(state *remoteState) bool {
	if len(scope.modules) == 0 {
		return true
	}
	for _, unique := range state.secret.Services {
		module, _, _ := strings.Cut(unique, "/")
		if slices.Contains(scope.modules, module) {
			return true
		}
	}
	return false
}

// property reports whether a property is planned, and whether only as the
// federation counterpart of the scope.
func (scope *planScope) property(state *remoteState, derivation solutionrun.SecretDerivation, derived bool) (inScope, counterpart bool) {
	if scope.owns(state) {
		scope.use(derivation, derived)
		return true, false
	}
	if !derived || !derivation.Digest {
		return false, false
	}
	for _, credential := range derivation.Credentials {
		if slices.Contains(scope.held, credential) {
			scope.use(derivation, derived)
			return true, true
		}
	}
	return false, false
}

func (scope *planScope) use(derivation solutionrun.SecretDerivation, derived bool) {
	if !derived {
		return
	}
	for _, credential := range derivation.Credentials {
		if !slices.Contains(scope.used, credential) {
			scope.used = append(scope.used, credential)
		}
	}
}

func (scope *planScope) written(derivation solutionrun.SecretDerivation) {
	target := &scope.writtenPlain
	if derivation.Digest {
		target = &scope.writtenDigest
	}
	for _, credential := range derivation.Credentials {
		if !slices.Contains(*target, credential) {
			*target = append(*target, credential)
		}
	}
}

// check refuses a plan that would store the digest of a credential it mints
// without also storing that credential where a service presents it: the
// registrar would then admit a secret no service holds, and the holder would
// later be minted a different one.
//
// The invariant is a credential's, not a scope's, so it is checked whether or
// not the plan is scoped. resolveCredentials already refuses to mint a
// credential no rendered key carries, so an unscoped plan should never reach
// this — it is the backstop for that rule, and the only enforcement for the
// scoped case, where the carrier is rendered but deliberately not planned.
func (scope *planScope) check(plan *Plan) error {
	for _, credential := range plan.Credentials {
		if credential.Action != ActionGenerate {
			continue
		}
		if !slices.Contains(scope.writtenDigest, credential.Credential) || slices.Contains(scope.writtenPlain, credential.Credential) {
			continue
		}
		if len(scope.modules) > 0 {
			return fmt.Errorf("--module %s would mint %s and store only its digest: the services holding it are outside the scope; add their module",
				strings.Join(scope.modules, ","), credential.Credential)
		}
		return fmt.Errorf("the plan would mint %s and store only its digest: nothing it writes hands that credential to a service, so the store would admit a secret no one holds",
			credential.Credential)
	}
	return nil
}

// credentialPlans keeps the credentials a planned property derives from.
func (scope *planScope) credentialPlans(all []CredentialPlan) []CredentialPlan {
	if len(scope.modules) == 0 {
		return all
	}
	var kept []CredentialPlan
	for _, credential := range all {
		if slices.Contains(scope.used, credential.Credential) {
			kept = append(kept, credential)
		}
	}
	return kept
}

// federationDerivations maps each remote key's properties to the federation
// derivation of the service reading it.
//
// The federation is keyed by the secret key a service reads — the CODEFLY__…
// name — never by the property the environment files that key under. Asking it
// about the property matches nothing whenever the two differ, and a federation
// credential the CLI derives itself then reports as one to type by hand.
func federationDerivations(states []*remoteState, federation solutionrun.DeployedSecrets) (map[string]map[string]solutionrun.SecretDerivation, error) {
	derivations := map[string]map[string]solutionrun.SecretDerivation{}
	for _, state := range states {
		for _, property := range state.secret.Properties {
			var found *solutionrun.SecretDerivation
			readers, err := propertyReaders(&state.secret, property)
			if err != nil {
				return nil, err
			}
			unconfigured := false
			for _, reader := range readers {
				derivation, ok := federation.For(reader.Service, reader.Key)
				if !ok {
					unconfigured = true
					continue
				}
				if found != nil && (!slices.Equal(found.Credentials, derivation.Credentials) || found.Encoded != derivation.Encoded || found.Digest != derivation.Digest) {
					return nil, fmt.Errorf("remote key %s property %s is read by services deriving it differently", state.secret.RemoteKey, property.Property)
				}
				found = &derivation
			}
			if found != nil && unconfigured {
				return nil, fmt.Errorf("remote key %s property %s mixes federation and configured sources", state.secret.RemoteKey, property.Property)
			}
			if found == nil {
				continue
			}
			if derivations[state.secret.RemoteKey] == nil {
				derivations[state.secret.RemoteKey] = map[string]solutionrun.SecretDerivation{}
			}
			derivations[state.secret.RemoteKey][property.Property] = *found
		}
	}
	return derivations, nil
}

// credentialValues are the federation credentials' plaintexts, which could not
// be established because a key that may hold one was not read, and which no
// rendered key carries at all.
type credentialValues struct {
	plaintexts map[solutionrun.Credential]string
	unverified map[solutionrun.Credential]bool
	// unheld are credentials no rendered ExternalSecret reads in plaintext, so
	// nothing this plan writes would ever present them. They are never minted:
	// see resolveCredentials.
	unheld map[solutionrun.Credential]bool
}

// resolveCredentials establishes every federation credential: the plaintext the
// store already holds, recovered from whichever key carries it, else a freshly
// minted one. Two keys carrying one credential with different values is a store
// that no longer federates, and is refused rather than silently resolved.
//
// A credential is minted only when some rendered ExternalSecret reads the key
// that would hold its plaintext. The federation is derived from the workspace
// and the keys from the render, and the two disagree whenever a module is not
// rendered for this environment — it renders for another one, or has not been
// rendered yet. Minting there would write the registrar the digest of a secret
// no service will ever be handed, and, because the registrar's digest list is
// recomputed whole, would drop the digest the module's running services are
// admitted by. So such a credential is reported as unheld and the properties
// deriving from it fall to `require`, leaving the stored value untouched.
func resolveCredentials(plan *Plan, states []*remoteState, derivations map[string]map[string]solutionrun.SecretDerivation, federation solutionrun.DeployedSecrets) (credentialValues, error) {
	values := credentialValues{plaintexts: map[solutionrun.Credential]string{}, unverified: map[solutionrun.Credential]bool{}, unheld: map[solutionrun.Credential]bool{}}
	heldIn := map[solutionrun.Credential]string{}
	// carried are the credentials some rendered key reads in plaintext, whether
	// or not that key was read or currently holds a value.
	carried := map[solutionrun.Credential]bool{}
	for _, state := range states {
		for property, derivation := range derivations[state.secret.RemoteKey] {
			if stored, present := state.document[property]; present {
				if err := derivation.ValidateStored(stored); err != nil {
					return credentialValues{}, fmt.Errorf("remote key %s property %s has an invalid federation value: %w", state.secret.RemoteKey, property, err)
				}
			}
			if derivation.Digest {
				continue
			}
			for _, credential := range derivation.Credentials {
				carried[credential] = true
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
		case !carried[credential]:
			values.unheld[credential] = true
			plan.Credentials = append(plan.Credentials, CredentialPlan{Credential: credential, Action: ActionRequire,
				Source: "no rendered ExternalSecret reads the key that would hold it: render its module for this environment, then seed again"})
		default:
			values.plaintexts[credential] = solutionrun.MintCredential()
			plan.Credentials = append(plan.Credentials, CredentialPlan{Credential: credential, Action: ActionGenerate, Source: "minted: no key holds it"})
		}
	}
	return values, nil
}

// planDerived plans a federation-derived property, returning the value to write
// when it changes.
func planDerived(state *remoteState, property gitops.RenderedSecretProperty, derivation solutionrun.SecretDerivation, credentials credentialValues) (PropertyPlan, string) {
	names := make([]string, 0, len(derivation.Credentials))
	inputsUnverified := false
	var unheld []string
	for _, credential := range derivation.Credentials {
		names = append(names, credential.String())
		inputsUnverified = inputsUnverified || credentials.unverified[credential]
		if credentials.unheld[credential] {
			unheld = append(unheld, credential.String())
		}
	}
	source := "derived from " + strings.Join(names, ", ")
	if derivation.Digest {
		source = "digests of " + strings.Join(names, ", ")
	}
	if !state.known() {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionUnverified, Source: source + " if absent or stale"}, ""
	}
	existing, present := state.document[property.Property]
	if inputsUnverified {
		action := ActionDerive
		if present {
			action = ActionUnverified
		}
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: action, Source: source + " (inputs not read)"}, ""
	}
	// A credential no rendered key holds cannot be minted here (resolveCredentials
	// says why), so this property is left exactly as the store has it.
	if len(unheld) > 0 {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire,
			Source: source + ": no rendered ExternalSecret reads the key holding " + strings.Join(unheld, ", ") +
				"; render its module for this environment, then seed again"}, ""
	}
	value, err := derivation.Value(credentials.plaintexts)
	if err != nil {
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire, Source: source + ": " + err.Error()}, ""
	}
	switch {
	case present && existing == value:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionKeep, Source: source}, ""
	case present:
		// The stored value is rewritten whole, so an identity it admits that this
		// derivation no longer covers would be dropped — de-authorizing whatever
		// holds it, which is a rotation of a stored value by another name. The
		// render is pinned to its inventory but the federation is derived from the
		// workspace as it is now, so the two diverge whenever the workspace moved
		// on: a consumed prefix renamed after the render leaves the old prefix
		// admitted here and derived nowhere. Refuse without quoting stored identities.
		if dropped := droppedIdentities(derivation, existing); len(dropped) > 0 {
			return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRequire,
				Source: source + ": the stored value also admits identities this workspace no longer derives; refusing to remove them (stored identities withheld)"}, ""
		}
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionUpdate, Source: source + " (stored value no longer encodes them)"}, value
	default:
		return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionDerive, Source: source}, value
	}
}

// droppedIdentities lists the identities a stored encoded value admits that the
// derivation no longer covers, so rewriting it would remove them.
func droppedIdentities(derivation solutionrun.SecretDerivation, stored string) []string {
	var dropped []string
	for _, identity := range derivation.Identities(stored) {
		if !slices.ContainsFunc(derivation.Credentials, func(credential solutionrun.Credential) bool { return credential.Identity == identity }) {
			dropped = append(dropped, identity)
		}
	}
	return dropped
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

// planConfigured plans a property that is not federation-derived.
//
// The other keys holding the same configuration value are looked at before this
// key's own state, not only when this key lacks it. A configuration value is one
// value however many services read it, so two keys holding it with different
// values is a store that no longer agrees with itself — and that is true whether
// or not a third key needs it propagated. Keeping what each key happens to hold
// would report the divergence as `keep`, which is the one reading an operator
// cannot act on.
func planConfigured(state *remoteState, property gitops.RenderedSecretProperty, states []*remoteState,
	generators map[string]environments.EnvironmentSecretGenerator, generated map[string]string, keys []string) (PropertyPlan, string, error) {
	lookup := property
	lookup.Keys = keys
	if _, _, err := declaredGenerator(lookup, generators); err != nil {
		return PropertyPlan{}, "", err
	}
	var holders, unread []string
	var holderValue string
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
				return PropertyPlan{}, "", fmt.Errorf("one configuration value cannot be two: different values in %s and %s; resolve it before planning", holders[0], location)
			}
			holders = append(holders, location)
			holderValue = value
		}
	}
	if state.known() {
		if stored, present := state.document[property.Property]; present {
			if len(holders) > 0 && stored != holderValue {
				return PropertyPlan{}, "", fmt.Errorf("%s holds different values in %s and %s: one configuration value cannot be two; resolve it before planning",
					strings.Join(property.Keys, ", "), state.secret.RemoteKey, holders[0])
			}
			return PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionKeep, Source: "stored"}, "", nil
		}
	}
	fallback, value, err := fallbackSource(lookup, generators, generated)
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
	default:
		return fallback, value, nil
	}
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
			Source: "no key holds it, no federation derivation covers it, and service-secrets.generate declares no generator for " +
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

// Apply writes the plan's changes. It refuses a plan that did not read the store
// — it would overwrite properties it never saw — and, unless allowMissing, one
// that leaves a property the operator must supply. Writes carrying plaintexts go
// before the registrar's digests that admit them. It returns the keys written,
// which on error are the ones written before it.
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
