package deploysecrets

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

// Two identities are in play here and conflating them is what makes a plan
// wrong, so they are named apart.
//
// The ADDRESSING identity is the ExternalSecret store reference a key is read
// through: kind, name, and — for the namespaced SecretStore kind — the
// namespace of the ExternalSecret that names it. It is what `kubectl get
// (cluster)secretstore` needs to resolve a backend at all, so it is what the
// render is split by before anything is resolved (StoreGroups).
//
// The BACKEND identity is the store a resolution landed on, as Store.Name()
// reports it. For the one provider this verb writes, that is "gcpsm project
// <projectID>" — the project IS the backend, so two different store references
// that resolve to one project are one backend, and planning them apart is how
// one configuration value ends up minted twice. Resolution is therefore
// per-reference and planning is per-backend, with the two reconciled by
// BuildAll below. A provider added later has to keep Name() identifying
// the backend rather than describing it, or that collapse stops working.
const kindNamespacedSecretStore = "SecretStore"

// StoreGroups splits a render by the store reference each remote key is read
// through, in a stable order: kind, then name, then namespace.
//
// Namespace is part of a SecretStore's identity because the kind is namespaced —
// the same name in two namespaces is two objects resolving through two backends
// — and is not part of a ClusterSecretStore's, which is shared across them.
func StoreGroups(rendered gitops.RenderedEnvironment) []gitops.RenderedEnvironment {
	type address struct {
		store     environments.EnvironmentSecretStoreReference
		namespace string
	}
	groups := map[address]gitops.RenderedEnvironment{}
	for _, secret := range rendered.Secrets {
		key := address{store: secret.Store}
		if secret.Store.Kind == kindNamespacedSecretStore {
			key.namespace = secret.Namespace
		}
		group, found := groups[key]
		if !found {
			group = gitops.RenderedEnvironment{Modules: rendered.Modules, Skipped: rendered.Skipped}
		}
		group.Secrets = append(group.Secrets, secret)
		groups[key] = group
	}
	keys := make([]address, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.store.Kind != b.store.Kind {
			return a.store.Kind < b.store.Kind
		}
		if a.store.Name != b.store.Name {
			return a.store.Name < b.store.Name
		}
		return a.namespace < b.namespace
	})
	result := make([]gitops.RenderedEnvironment, 0, len(groups))
	for _, key := range keys {
		result = append(result, groups[key])
	}
	return result
}

// BackendPlan is one backend's plan and the share of the render it covers.
type BackendPlan struct {
	Store    Store
	Rendered gitops.RenderedEnvironment
	Plan     *Plan
}

// BuildAll resolves every property of a render that reads through several
// backends, and returns one plan per backend.
//
// Reads and writes are per backend: no value is ever copied from one backend to
// another, which is what a per-key store declaration asks for. What is NOT per
// backend is the identity of a configuration value. A configuration key is one
// value however many properties file it and however many backends hold it, so:
//
//   - a value this run must mint is minted ONCE for the stored key and handed to
//     every backend that reads it. Minting per backend would write two different
//     values for one key, which no later run could even report, because each
//     would see only its own backend agreeing with itself;
//   - a value two backends already hold differently is refused, naming both,
//     rather than reported as `keep` on each side;
//   - a value one backend holds and another lacks is REQUIRED in the one that
//     lacks it, naming where it already is. It is deliberately neither copied
//     (that would move a value between backends) nor minted (that would be the
//     divergence above).
//
// resolve is called once per store reference the render names, and only for a
// reference some key of the run's own --module scope reads: a scoped run has no
// business holding credentials for, or reading payloads out of, a backend it
// will never write.
func BuildAll(ctx context.Context, in *Inputs,
	resolve func(gitops.RenderedEnvironment) (Store, error)) ([]*BackendPlan, error) {
	if err := validateRenderedProperties(in.Rendered); err != nil {
		return nil, err
	}
	var backends []*resolvedBackend
	byIdentity := map[string]*resolvedBackend{}
	for _, group := range StoreGroups(in.Rendered) {
		if !planningAny(group.Secrets, in.Modules) {
			continue
		}
		store, err := resolve(group)
		if err != nil {
			return nil, err
		}
		if store == nil {
			return nil, fmt.Errorf("no store resolved for the render read through %s/%s",
				group.Secrets[0].Store.Kind, group.Secrets[0].Store.Name)
		}
		// Two store references that resolved to one backend are one plan: see the
		// identity note at the top of this file.
		if existing, found := byIdentity[store.Name()]; found {
			existing.Rendered.Secrets = mergeRenderedSecrets(existing.Rendered.Secrets, group.Secrets)
			continue
		}
		target := &resolvedBackend{Store: store, Rendered: group}
		byIdentity[store.Name()] = target
		backends = append(backends, target)
	}

	generators, err := generatorIndex(in.Generators, in.Services)
	if err != nil {
		return nil, err
	}
	// Every backend is read before any is planned: the one-value rules above are
	// about what the backends hold together, so no plan can be resolved from one
	// backend's view alone.
	var all []*remoteState
	local := make([][]*remoteState, len(backends))
	for i, target := range backends {
		states, err := readStates(ctx, in, target.Store, target.Store.Name(), target.Rendered.Secrets)
		if err != nil {
			return nil, err
		}
		local[i] = states
		all = append(all, states...)
	}
	aliases := configurationAliases(all)
	// One value per stored key, shared by every backend that mints it.
	generated := map[string]string{}

	plans := make([]*BackendPlan, 0, len(backends))
	for i, target := range backends {
		plan, err := planBackend(in, target.Store.Name(), local[i], all, generators, generated, aliases)
		if err != nil {
			return nil, err
		}
		plans = append(plans, &BackendPlan{Store: target.Store, Rendered: target.Rendered, Plan: plan})
	}
	return plans, nil
}

// resolvedBackend is one resolved store with every rendered key that reads
// through it, however many store references addressed it.
type resolvedBackend struct {
	Store    Store
	Rendered gitops.RenderedEnvironment
}

// mergeRenderedSecrets unions two shares of one backend by remote key. Without
// it the same key would be described, read and written twice in one plan, and
// the two writes would race over one document. The readers and the properties of
// a key both shares name are the same key's, and a property read under two keys
// keeps both.
func mergeRenderedSecrets(into, from []gitops.RenderedServiceSecret) []gitops.RenderedServiceSecret {
	index := map[string]int{}
	for i := range into {
		index[into[i].RemoteKey] = i
	}
	for _, secret := range from {
		at, found := index[secret.RemoteKey]
		if !found {
			index[secret.RemoteKey] = len(into)
			into = append(into, secret)
			continue
		}
		target := &into[at]
		for _, service := range secret.Services {
			if !slices.Contains(target.Services, service) {
				target.Services = append(target.Services, service)
			}
		}
		sort.Strings(target.Services)
		target.Properties = mergeRenderedProperties(target.Properties, secret.Properties)
	}
	return into
}

func mergeRenderedProperties(into, from []gitops.RenderedSecretProperty) []gitops.RenderedSecretProperty {
	index := map[string]int{}
	for i := range into {
		index[into[i].Property] = i
	}
	for _, property := range from {
		at, found := index[property.Property]
		if !found {
			index[property.Property] = len(into)
			into = append(into, property)
			continue
		}
		target := &into[at]
		for _, key := range property.Keys {
			if !slices.Contains(target.Keys, key) {
				target.Keys = append(target.Keys, key)
			}
		}
		sort.Strings(target.Keys)
		for _, reader := range property.Readers {
			if !slices.Contains(target.Readers, reader) {
				target.Readers = append(target.Readers, reader)
			}
		}
	}
	sort.Slice(into, func(i, j int) bool { return into[i].Property < into[j].Property })
	return into
}

// planningAny reports whether any of these keys is one the run would write, so a
// backend the --module scope reaches nothing in is never resolved or read.
func planningAny(secrets []gitops.RenderedServiceSecret, modules []string) bool {
	for i := range secrets {
		if planning(&secrets[i], modules) {
			return true
		}
	}
	return false
}
