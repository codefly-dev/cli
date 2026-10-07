package orchestration

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// The workspace configuration groups a service receives are resolved here, once,
// for every path that delivers them. There is one rule:
//
//	the groups a service receives = the groups it declares
//	    (`workspace-configuration-dependencies`)
//	  ∪ the groups the composition root provides run-wide
//	    (core's GetCompositionRootWorkspaceConfigurations)
//	  − the groups the selected run profile excludes
//
// The rendered deployment is the source of truth for that set, and the local run
// resolves the identical one. The two must not differ in either direction. A
// render that resolved less than a run makes "works locally, dials something
// unconfigured once deployed" a property of the pipeline rather than a mistake
// anyone made: the composition root supplies a value every service can read, the
// run delivers it, and the deployed service started without it — naming a key it
// never saw, or not noticing. A run that resolved less than a render would hide
// the opposite fault just as well, so neither path may carry a resolution of its
// own. Only the address family differs, and legitimately: the same group gives a
// native consumer a loopback address and a deployed one its in-cluster address.
//
// The set is selected *before* anything is resolved, and the whole of it drives
// producer discovery. That order is the correctness condition, not a tidiness
// one: core resolves the root's run-wide groups leniently — a value whose
// ${endpoint:…} this consumer cannot resolve is dropped for it rather than
// failing it (#393), and an information block every value of which was dropped
// goes with them. Discovering producers from the declared groups alone therefore
// deletes a root group's endpoint values, and a root group holding nothing else
// disappears whole, with no error anywhere: exactly the omission above, in a new
// place. See effectiveWorkspaceConfigurationGroups and
// refuseDroppedWorkspaceConfigurationValues.
//
// One asymmetry is deliberate and directional: a run profile's
// `exclude-workspace-configurations` trims the run only — profiles never reach a
// build or a deployment — so a profile can leave the run with fewer groups than
// the render, never the reverse, and cannot produce the fault above.
//
// Both delivery paths are below (Runner.workspaceConfigurations,
// Builder.workspaceConfigurations) so that a change to one is a change made in
// sight of the other. The rule is stated for operators in
// `docs/orchestration.md` ("Workspace configuration groups").

// workspaceConfigurationsFor resolves the workspace configurations one service
// receives. ${endpoint:…} references resolve against that service's dependency
// mappings, in the address family of its access, plus the mappings of every
// producer in the run that a configuration of the service's *effective* group
// set names — the groups it declares and the root's alike (see
// referencedProducerMappings).
func (world *World) workspaceConfigurationsFor(
	ctx context.Context, service *resources.Service,
	dependencyMappings []*basev0.NetworkMapping, access *basev0.NetworkAccess,
) ([]*basev0.Configuration, error) {
	declared := make([]string, 0, len(service.WorkspaceConfigurationDependencies))
	for _, dependency := range service.WorkspaceConfigurationDependencies {
		if !world.excludedWorkspaceConfigurations[dependency] {
			declared = append(declared, dependency)
		}
	}
	// The complete set first: a reference carried by a group the service never
	// declared resolves only if the producer it names was discovered, and only
	// this set knows about it.
	effective := world.effectiveWorkspaceConfigurationGroups(declared)
	// What this service will NOT receive, decided BEFORE anything is checked or
	// resolved. A composition root's credentials do not reach a service that
	// does not declare the group, and a value nobody receives must not impose an
	// obligation on them: deciding at delivery time meant a withheld
	// credential's ${endpoint:…} was still validated and still had to resolve,
	// so a reference this consumer could never read could refuse its render —
	// its producer's address undiscoverable, or its endpoint private to another
	// module. One decision, made once, used by the check, by discovery and by
	// the delivery filter. (Layer-4 round-five F4.)
	// Before anything is resolved: a World that cannot say which of the groups
	// it loaded are the composition root's cannot plan for them either.
	if err := world.requireRootGroupSource(declared); err != nil {
		return nil, err
	}
	withheld := world.withheldRootCredentials(declared, effective)
	// Validity next, and by core's own rule: a malformed reference, a producer
	// the workspace does not have, an endpoint it does not declare, or an
	// endpoint the consumer's module may not see is refused here for the whole
	// effective set — not only for the groups this service declared.
	if err := world.checkEffectiveWorkspaceConfigurationReferences(ctx, service, effective, withheld); err != nil {
		return nil, err
	}
	// The consumer's OWN dependency mappings are filtered too, and before
	// discovery runs against them. A bare service dependency — one naming no
	// endpoints — used to be handed every mapping its producer published,
	// narrowed by the dependency's endpoint list and not by visibility
	// (StateManager.GetDependenciesNetworkMappings), so a cross-module private
	// endpoint's address was in the set a ${endpoint:…} resolves against. And
	// because discovery skips a producer whose endpoint the dependency mappings
	// already carry, filtering only what discovery binds meant the filter never
	// ran at all for exactly that consumer. (Layer-5 round-five NEW-1.) The
	// hand-out is judged with the composition's provenance since core v0.14.0,
	// so this filter is a second pass over it; it stays because it is the one
	// that also covers what discovery binds.
	visible, err := world.exportableTo(ctx, service, dependencyMappings)
	if err != nil {
		return nil, err
	}
	referenced, err := world.referencedProducerMappings(ctx, service, declared, effective, visible, withheld)
	if err != nil {
		return nil, err
	}
	mappings := append(slices.Clone(visible), referenced...)
	// Which endpoint a reference names is CORE's answer, not this package's.
	//
	// Core selects the endpoint per reference — the exact name wins, an exact
	// name this consumer may not reach is refused rather than replaced by a
	// permitted API sibling, an ambiguous reference is refused, and only the
	// selected endpoint is bound — so the ordering, removal and verification
	// this package used to run between discovery and resolution is gone. It
	// could only ever model core's scan: each review round found the model
	// diverging from it somewhere new, and the last one in an interaction
	// between two individually-correct rules. core#702 moved the answer to
	// where the scan lives, and this hands it the two inputs it cannot read off
	// the mappings: who the consumer is, and what each producer declares.
	// The consumer's module when it is readable, and empty when it is not.
	//
	// Core requires it to SELECT an endpoint and refuses with
	// ErrConsumerNotIdentified when a reference needs selecting without it — so
	// the fail-closed behaviour is core's and does not need repeating here.
	// Refusing at this point instead failed every resolution of a service whose
	// identity carries no module, including the ones whose configurations hold
	// no reference at all and for which the module is never consulted: runtime
	// init and accepted-port publication both resolve configurations that
	// select nothing.
	consumerModule := ""
	if identity, idErr := service.Identity(); idErr == nil {
		consumerModule = identity.Module
	}
	declaredEndpoints, err := world.declaredEndpointsLookup(ctx)
	if err != nil {
		return nil, err
	}
	manager := world.ConfigurationManager.ForConsumer(mappings, access).
		ForConsumerModule(consumerModule, declaredEndpoints).
		WithRunProducers(world.producerInRun())
	resolved, err := manager.GetWorkspaceDependenciesConfigurations(ctx, declared...)
	if err != nil {
		return nil, err
	}
	// The composition root injects its own workspace configurations into every
	// service, so a composed-module service resolves root-provided values
	// without redeclaring them as dependencies. A service that declares a
	// dependency on one of the root's own configurations (e.g. the root service
	// itself) yields that name in both sets, so union by name to avoid emitting
	// it twice.
	root, err := manager.GetCompositionRootWorkspaceConfigurations(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(resolved))
	for _, conf := range resolved {
		for _, info := range conf.Infos {
			seen[info.Name] = true
		}
	}
	out := resolved
	for _, conf := range root {
		if world.workspaceConfigurationExcluded(conf) || world.workspaceConfigurationSeen(conf, seen) {
			continue
		}
		if err = world.requireKnownRootGroup(conf, effective); err != nil {
			return nil, err
		}
		kept := withheld.without(ctx, conf, consumerLabel(service))
		if kept == nil {
			continue
		}
		out = append(out, kept)
	}
	out = world.applyWorkspaceConfigurationValues(out, declared)
	// Last, because it reads the OUTCOME: every value that carried a reference
	// must still be here. Core resolves the root's groups leniently and drops
	// what a consumer cannot satisfy, so this is the only place a drop is
	// visible at all — and the only honest way to judge it, since whether
	// core's interpolation succeeds is core's to know.
	//
	// A credential withheld above is absent on purpose, so it is passed in
	// rather than discovered missing: a root credential whose value carries an
	// ${endpoint:…} would otherwise be reported as a lost value and refuse the
	// render it was deliberately kept out of.
	if err = world.refuseDroppedWorkspaceConfigurationValues(ctx, service, effective, out, withheld.values); err != nil {
		return nil, err
	}
	return out, nil
}

// withheldCredentials is what one service does NOT receive of the composition
// root's groups: its credentials. Computed once, before anything is checked or
// resolved, and then used by the reference check, by producer discovery, by the
// delivery filter and by the drop detection — so the four cannot disagree about
// which values this service is getting.
//
// It is reached only for a group the service did not declare: a declared group
// resolves through GetWorkspaceDependenciesConfigurations and never comes
// through here, and a group that is both is skipped as already seen. So the rule
// an operator meets is exactly: a root group's non-secret values reach every
// service, and its credentials reach the services that ask for them.
//
// This is a deliberate narrowing of least privilege, and it is the one place
// where this PR does NOT make a deployed service receive what a run gives it —
// because it takes the value away from both. Making the render deliver the root's
// groups (the #882 fix) would otherwise have handed every workload in the
// composition every root credential as a mandatory secretKeyRef, so compromising
// any one service would yield all of them, and the environment's store would
// hold a copy of each credential per service. Before this PR a render delivered
// declared groups only, so nothing was widened on the credential axis; the fix
// must not widen it either. Parity is kept because both paths withhold: a
// service that needs a root credential declares the group, in
// `workspace-configuration-dependencies`, and gets it in the run and in the
// render alike.
//
// "Credential" is the render's own classifier, not a new one: an explicit
// `Secret` flag or a credential-named key (resources.IsSensitiveKey), which is
// exactly what promotes a value to a secretKeyRef (restrictedRenderRejects,
// promotableConfiguration) and what `deploy secrets` plans a store entry for. A
// narrower test — the flag alone — would leave a credential-named value, which
// is the shape an operator most often writes, reaching every workload as a
// secret reference.
//
// A STRUCTURED secret — `<name>.secret.yaml`, which core loads as an
// information block carrying Data{Secret: true} and typically no values at all —
// is withheld whole, by group name. Classifying values alone missed it
// entirely: with no values there was nothing to withhold, so the group reached
// every service in a run and in a render. It was also a render regression,
// because promotableConfiguration refuses a structured secret ("requires typed
// Kubernetes key references"), so a composition whose root `.secret.yaml` no
// service declared could not render ANY service.
type withheldCredentials struct {
	// values are the "<group>/<key>" entries this service does not receive.
	values map[string]bool
	// groups are the information blocks withheld whole — the structured secrets.
	groups map[string]bool
}

// any reports whether anything is withheld at all, so the common case costs
// nothing.
func (w withheldCredentials) any() bool {
	return len(w.values) > 0 || len(w.groups) > 0
}

// withheldRootCredentials decides, from the configurations as they were LOADED,
// which values of the effective set this service will not receive: the
// credentials of the groups only the composition root provides.
//
// Read pre-interpolation, like every other judgement in this file that has to
// know what a value IS rather than what it became.
func (world *World) withheldRootCredentials(declared, effective []string) withheldCredentials {
	if world == nil || world.providedWorkspaceConfigurationInfos == nil {
		return withheldCredentials{values: map[string]bool{}, groups: map[string]bool{}}
	}
	rootOnly := make(map[string]bool, len(effective))
	for _, group := range effective {
		rootOnly[group] = true
	}
	for _, group := range declared {
		delete(rootOnly, group)
	}
	return credentialsOf(world.providedWorkspaceConfigurationInfos(), rootOnly)
}

// credentialsOf is the one reading of "which values of these groups are
// credentials", shared by the resolution and by the plan gate. Two readings
// would be two answers to "what does this service receive", and the gate's
// answer decides whether a plan is refused.
func credentialsOf(infos []*basev0.ConfigurationInformation, groups map[string]bool) withheldCredentials {
	out := withheldCredentials{values: map[string]bool{}, groups: map[string]bool{}}
	if len(groups) == 0 {
		return out
	}
	for _, info := range infos {
		if !groups[info.GetName()] {
			continue
		}
		structured := info.GetData().GetSecret()
		if structured {
			out.groups[info.GetName()] = true
		}
		for _, value := range info.GetConfigurationValues() {
			if structured || isCredentialValue(value) {
				out.values[info.GetName()+"/"+value.GetKey()] = true
			}
		}
	}
	return out
}

// without returns conf as this service receives it: the withheld values and the
// withheld information blocks removed.
//
// The configuration is rebuilt rather than edited: it is core's, shared by every
// service's resolution, and a value removed in place would be removed for the
// consumer that declared the group too. nil means the whole configuration is
// withheld.
func (w withheldCredentials) without(ctx context.Context, conf *basev0.Configuration, consumer string) *basev0.Configuration {
	if !w.any() {
		return conf
	}
	removed := false
	infos := make([]*basev0.ConfigurationInformation, 0, len(conf.GetInfos()))
	for _, info := range conf.GetInfos() {
		if w.groups[info.GetName()] {
			removed = true
			wool.Get(ctx).In("World.workspaceConfigurationsFor").Debug(
				"withholding a composition-root structured secret from a service that does not declare its group",
				wool.Field("consumer", consumer), wool.Field("group", info.GetName()),
				wool.Field("remedy", "declare the group in workspace-configuration-dependencies to receive it"))
			continue
		}
		values := make([]*basev0.ConfigurationValue, 0, len(info.GetConfigurationValues()))
		for _, value := range info.GetConfigurationValues() {
			if !w.values[info.GetName()+"/"+value.GetKey()] {
				values = append(values, value)
				continue
			}
			removed = true
			wool.Get(ctx).In("World.workspaceConfigurationsFor").Debug(
				"withholding a composition-root credential from a service that does not declare its group",
				wool.Field("consumer", consumer), wool.Field("group", info.GetName()),
				wool.Field("key", value.GetKey()),
				wool.Field("remedy", "declare the group in workspace-configuration-dependencies to receive it"))
		}
		if len(values) == 0 && len(info.GetConfigurationValues()) > 0 {
			// A group all of whose values are credentials is not delivered as an
			// empty group: an empty information block is a group the service
			// "has" with nothing in it, which reads as a configuration fault
			// rather than as a boundary.
			removed = true
			continue
		}
		infos = append(infos, &basev0.ConfigurationInformation{
			Name:                info.GetName(),
			Data:                info.GetData(),
			ConfigurationValues: values,
		})
	}
	if !removed {
		return conf
	}
	if len(infos) == 0 {
		return nil
	}
	return &basev0.Configuration{Origin: conf.GetOrigin(), Infos: infos}
}

// received returns the information blocks as this service receives them, for
// the checks that must judge only what it gets: the withheld blocks dropped and
// the withheld values removed. The blocks are rebuilt, never edited — they are
// the loader's own.
func (w withheldCredentials) received(infos []*basev0.ConfigurationInformation) []*basev0.ConfigurationInformation {
	if !w.any() {
		return infos
	}
	out := make([]*basev0.ConfigurationInformation, 0, len(infos))
	for _, info := range infos {
		if w.groups[info.GetName()] {
			continue
		}
		values := make([]*basev0.ConfigurationValue, 0, len(info.GetConfigurationValues()))
		for _, value := range info.GetConfigurationValues() {
			if w.values[info.GetName()+"/"+value.GetKey()] {
				continue
			}
			values = append(values, value)
		}
		out = append(out, &basev0.ConfigurationInformation{
			Name:                info.GetName(),
			Data:                info.GetData(),
			ConfigurationValues: values,
		})
	}
	return out
}

// skips reports the ${endpoint:…} references no value this service receives
// carries — the ones carried ONLY by withheld credentials. Producer discovery
// and the reference check both leave those alone: a value this service will
// never read must not be able to refuse its run or its render, which is what
// imposing the obligation before deciding the delivery did.
func (w withheldCredentials) skips(referencing map[string][]string) map[string]bool {
	if !w.any() {
		return nil
	}
	carried := map[string]int{}
	byWithheld := map[string]int{}
	for qualified, references := range referencing {
		for _, reference := range references {
			carried[reference]++
			if w.values[qualified] {
				byWithheld[reference]++
			}
		}
	}
	out := map[string]bool{}
	for reference, count := range carried {
		if count > 0 && count == byWithheld[reference] {
			out[reference] = true
		}
	}
	return out
}

// isCredentialValue reports whether a value is one the render would promote to a
// secretKeyRef and `deploy secrets` would plan a store entry for: an explicit
// Secret flag, or a credential-named key. See restrictedRenderRejects, which
// mirrors core's own guard and keys off the same marker list.
func isCredentialValue(value *basev0.ConfigurationValue) bool {
	return value.GetSecret() || resources.IsSensitiveKey(value.GetKey())
}

// workspaceConfigurationInfos flattens a loader's configurations into the
// information blocks core's reference check reads. Every workspace
// configuration carries its group name on each Info.
func workspaceConfigurationInfos(confs []*basev0.Configuration) []*basev0.ConfigurationInformation {
	var out []*basev0.ConfigurationInformation
	for _, conf := range confs {
		if conf.GetOrigin() != resources.ConfigurationWorkspace {
			continue
		}
		out = append(out, conf.GetInfos()...)
	}
	return out
}

// checkEffectiveWorkspaceConfigurationReferences holds every ${endpoint:…} in a
// service's *effective* group set to the rules core applies to a declared
// one: the reference is well formed, the producer is a service of the
// workspace, it declares the endpoint named, and that endpoint is visible to
// the consumer's module.
//
// This closes the hole this PR opened. Core's plan-time check
// (configurations.CheckEndpointReferences, reached through
// CheckConfigurationReferences) iterates consumer.WorkspaceConfigurationDependencies
// — the DECLARED groups only. Its own comment gives the reason the visibility
// rule is there at all: "without this, declaring the group instead of the
// dependency would be the way around visibility". Before this PR a service that
// did not declare a root group never had the producer's mappings bound, so the
// value was dropped and the boundary held by accident. Now every root-referenced
// producer is bound for every service, so a root group carrying
// ${endpoint:platform/authority/admin} on an endpoint whose visibility is
// `private` would reach every service of the composition with nothing refusing
// it, and a typo'd producer in a root group would be dropped from every service
// in silence — the #882 fault itself, in a new place. (Since core v0.14.0
// `internal` names nobody and permits every module of the composition — the
// allow-list is derived from declared dependencies and an authored one is
// refused at load — so `private` is the one reach that refuses; the former
// `module` is refused at load too.)
//
// It is core's rule, not a second one: core's exported check is called, with the
// consumer's declared set replaced by the effective set. That replacement is the
// shim, and the durable fix is in core — let
// configurations.CheckEndpointReferences take the effective group set, so the
// plan-time check and this resolution stop being two selections. Named in
// `docs/orchestration.md` and in the PR body.
//
// What this does NOT establish, and an earlier revision of this comment claimed
// it did: that the check and the resolution cannot disagree about which endpoint
// a reference names. They can. Core's check returns on the first manifest
// endpoint a reference matches; core's interpolation takes the first bound
// mapping that matches and has an instance for the consumer's access. A
// reference naming an API can match several of a producer's endpoints, so the
// check can pass on a public one while the resolution hands over a private
// one's address. Holding the check to the effective set closes the group-as-a-way-
// around-visibility hole and nothing else; the endpoint-selection hole is closed
// on the binding side, in World.exportableTo, which is where the set the
// resolution walks is built.
//
// Whether an address EXISTS is deliberately not judged here — that is
// judgeDroppedValue's job, from the outcome, and it asks a different question
// per path: a render asks whether the workspace has the producer at all, a run
// asks whether this run contains it. The lookup here is therefore workspace-wide
// in both: the dependency graph would report an excluded producer, or one
// outside a module-closure run, as "not a service of this workspace", which is a
// different fault with a different fix.
func (world *World) checkEffectiveWorkspaceConfigurationReferences(
	ctx context.Context, service *resources.Service, effective []string, withheld withheldCredentials,
) error {
	if world == nil || len(effective) == 0 {
		return nil
	}
	if world.providedWorkspaceConfigurationInfos == nil {
		// Refuse rather than skip, and only when there is something to check.
		// An unbound source made both this check and the drop detection below
		// no-ops, so every reference in the effective set went unvalidated and
		// every dropped value went unnoticed — silently, which is the fault
		// this file exists to remove. NewFlow binds it beside the loader;
		// anything else that resolves groups must too.
		// The COUNT, never the references. A refusal is a diagnostic, and a
		// diagnostic that quotes ${endpoint:…} republishes the very producer
		// and endpoint the check was meant to adjudicate — to a reader who may
		// be exactly the consumer that must not learn it. There is no position
		// to report here: the capability that would resolve positions is the
		// missing one.
		if count := len(world.ConfigurationManager.WorkspaceEndpointReferences(effective...)); count > 0 {
			return fmt.Errorf("cannot validate %d workspace configuration endpoint reference(s): this world resolves groups but was built without the configurations they were loaded from, so a reference naming a private endpoint or a producer that does not exist would be delivered unchecked and a dropped value would go unnoticed (bind World.providedWorkspaceConfigurationInfos as NewFlow does)",
				count)
		}
		return nil
	}
	// Only what this service receives. A withheld credential's reference is not
	// this consumer's to satisfy, so validating it here would let a value it
	// will never read refuse its run — on a private endpoint, say, that the
	// service declaring the group may perfectly well reach.
	infos := withheld.received(world.providedWorkspaceConfigurationInfos())
	if len(infos) == 0 {
		return nil
	}
	lookup, err := world.workspaceProducers(ctx)
	if err != nil {
		return err
	}
	if lookup == nil {
		// No workspace, so no producer can be looked up and no reference can be
		// validated — and this is the last fail-open in this file. A World built
		// without a workspace used to resolve reference-bearing groups with no
		// check at all, so a mapping for a private endpoint of another module
		// was delivered with err=nil: the same bypass as every other case here,
		// reached by leaving out the thing that answers the question. NewFlow
		// always binds a workspace, so this refuses a construction no production
		// path takes rather than a real one — which is exactly why it should
		// refuse instead of being trusted to stay unreachable.
		// The count, for the reason given at the guard above.
		if count := len(world.ConfigurationManager.WorkspaceEndpointReferences(effective...)); count > 0 {
			return fmt.Errorf("cannot validate %d workspace configuration endpoint reference(s): this world resolves groups but has no workspace, so a reference naming a private endpoint or a producer that does not exist cannot be checked against anything (bind World.Workspace as NewFlow does)",
				count)
		}
		return nil
	}
	// A shallow copy: core reads WorkspaceConfigurationDependencies off the
	// consumer, and the service it was handed must not be mutated — it is the
	// workspace's own object, shared by every resolution.
	consumer := *service
	consumer.WorkspaceConfigurationDependencies = effective
	// A zero profile excludes nothing: `effective` has already had the run
	// profile's exclusions removed, and passing them twice would only hide a
	// group from a check it has to pass.
	// Core's check is the whole check. It refuses a malformed reference, a
	// producer the workspace does not have, an endpoint the producer does not
	// declare, an endpoint this consumer may not reach — and, since core#702,
	// an ambiguous one, because it asks the same selection the resolution asks.
	// This package used to run its own ambiguity rule beside it, from the days
	// when core's check judged the FIRST matching endpoint and its resolution
	// bound the first matching mapping; there is one answer now, and a second
	// opinion here could only disagree with it.
	return configurations.CheckEndpointReferences(infos, []*resources.Service{&consumer}, resources.RunProfile{}, lookup)
}

// A nonexistent producer is refused here too, not left to the plan gate.
//
// An earlier revision tolerated it: the read dropped the value with a WARN and
// the gate refused the plan, which is the division this package has for declared
// groups. Dynamic review showed that to be a fail-open rather than a division,
// because it makes the refusal depend on another gate having run over the same
// values — and one does not always. A typo supplied through an
// invocation-scoped override reached the
// resolution while the gate was still reading the pre-override configurations,
// so nothing refused it anywhere and the value was simply dropped. The gate is
// invocation-aware now (WorkspaceConfigurationsForChecking), but a guard that is
// only correct while a second guard is also correct is not a guard.
//
// So core's verdict is propagated whole. What stays distinct is the thing that
// genuinely differs: a producer that EXISTS but is outside this run or this
// deployment, which judgeDroppedValue decides from the outcome — a drop in a
// run, a refusal in a render. "The producer does not exist" is never that.

// workspaceProducers resolves <module>/<service> over the whole workspace,
// memoized. It is the authority for "that producer does not exist", which the
// run set and the dependency graph cannot give: both omit an excluded producer
// exactly as they omit a service of another workspace.
//
// A World with no workspace (a unit test resolving a hand-built group) gets no
// lookup and no check rather than a wrong verdict.
// declaredEndpointsLookup is what each producer of the workspace DECLARES, for
// core's per-reference endpoint selection.
//
// It comes from the same single read of the workspace's services as the plan
// gate's producer lookup, through core's own resources.DeclaredEndpointsOf, so
// the manifest the resolution selects against and the manifest the check judges
// against are one source rather than two that have to be kept in step.
//
// Core refuses a resolution whose selection context is incomplete, so a nil
// answer here is a refusal and not a lenient fallback: a reference resolved
// against no manifest is a reference whose visibility nothing judged.
func (world *World) declaredEndpointsLookup(ctx context.Context) (resources.DeclaredEndpoints, error) {
	if _, err := world.workspaceProducers(ctx); err != nil {
		return nil, err
	}
	return world.workspaceDeclaredEndpoints, nil
}

func (world *World) workspaceProducers(ctx context.Context) (configurations.ProducerLookup, error) {
	if world == nil || world.Workspace == nil {
		return nil, nil
	}
	// One implementation, memoized here: workspaceProducerLookup is the plan
	// gate's lookup too, and two bodies answering "is this a service of the
	// workspace" is how the gate and the resolution come to disagree about a
	// producer.
	world.workspaceProducerLookupOnce.Do(func() {
		world.workspaceProducerLookup, world.workspaceDeclaredEndpoints, world.workspaceProducerLookupErr =
			workspaceProducers(ctx, world.Workspace)
	})
	if world.workspaceProducerLookupErr != nil {
		// Fail closed, and keep failing: the failure is memoized for the whole
		// world, so a Debug line here would silence every reference check of
		// every service of the run from one unreadable workspace. A reference
		// delivered unchecked is how a private endpoint's address reaches a
		// service that may not see it.
		return nil, world.workspaceProducerLookupErr
	}
	return world.workspaceProducerLookup, nil
}

// effectiveWorkspaceConfigurationGroups is the group set one service receives,
// by name: the groups it declares unioned with the composition root's own,
// minus the profile exclusions, each name once and in a stable order (declared
// first, then the root's, sorted). It is what a resolution must be planned
// against — producer discovery above, and every check that follows.
//
// The root's names are read from the loaders the manager was built with, through
// core's own capability (CompositionRootWorkspaceConfigurationNames), lazily:
// they are populated by the loader's Load, which runs after the World exists.
// A World built without that source names no root group; requireKnownRootGroup
// is what keeps that from passing silently.
func (world *World) effectiveWorkspaceConfigurationGroups(declared []string) []string {
	effective := slices.Clone(declared)
	seen := make(map[string]bool, len(declared))
	for _, group := range declared {
		seen[group] = true
	}
	// Cloned before sorting: the slice is the loader's own, shared by every
	// resolution of the run, and sorting in place would reorder the loader's
	// state as a side effect of reading it. (Not a data race: the playbook
	// executes its actions one at a time — playbook.go — so resolutions do not
	// overlap. An earlier revision of this comment said they did.)
	root := slices.Clone(world.compositionRootWorkspaceConfigurationGroups())
	slices.Sort(root)
	for _, group := range root {
		if seen[group] || world.excludedWorkspaceConfigurations[group] {
			continue
		}
		seen[group] = true
		effective = append(effective, group)
	}
	return effective
}

// compositionRootWorkspaceConfigurationGroups names the composition root's own
// groups, as the manager's loaders report them.
func (world *World) compositionRootWorkspaceConfigurationGroups() []string {
	if world == nil || world.compositionRootGroups == nil {
		return nil
	}
	return world.compositionRootGroups()
}

// requireRootGroupSource refuses to resolve anything for a World that was built
// without a composition-root group source while its loader holds groups this
// service did not declare.
//
// It checks the CONDITION, where requireKnownRootGroup below checks a symptom —
// and one symptom only. That guard inspects the information blocks the
// resolution delivered, so a root group the World could not name AND whose every
// value core's interpolation dropped leaves nothing to inspect: the block is
// gone, `effective` never held the group, and the outcome check has no name to
// miss it by either. A group that is nothing but an unresolvable ${endpoint:…}
// is exactly that shape, and it is the shape most likely to matter.
//
// The two signals come from one loader in NewFlow — the configurations and the
// root names — so a World holding the first without the second is the
// inconsistency, not its consequences. Refusing it here covers every symptom at
// once, before interpolation can erase the evidence. A World whose source is
// bound and reports no root groups is a different thing and is fine: the
// function pointer is what is checked, not its result. (Layer-2 round-six
// finding 5.)
func (world *World) requireRootGroupSource(declared []string) error {
	if world == nil || world.compositionRootGroups != nil || world.providedWorkspaceConfigurationInfos == nil {
		return nil
	}
	own := make(map[string]bool, len(declared))
	for _, group := range declared {
		own[group] = true
	}
	for _, info := range world.providedWorkspaceConfigurationInfos() {
		if own[info.GetName()] {
			continue
		}
		return fmt.Errorf("cannot resolve workspace configurations: this world was loaded with the group %q, which this service does not declare, but it has no composition-root group source, so it cannot tell whether that group is provided run-wide — and a root group it cannot name is one producer discovery never plans for, whose values go missing in silence. Bind World.compositionRootGroups as NewFlow does (configurations.Loader.CompositionRootWorkspaceConfigurationNames)",
			info.GetName())
	}
	return nil
}

// requireKnownRootGroup refuses a root group the resolution delivered that the
// effective set did not name. It can only happen when the world's root-name
// source is incomplete — and that source is what producer discovery planned
// against, so every ${endpoint:…} the group carries was resolved against
// mappings nobody bound for it. Shipping the group anyway is shipping the
// omission: a value silently absent from a deployed workload. Refuse instead,
// naming the group and the cause.
func (world *World) requireKnownRootGroup(conf *basev0.Configuration, effective []string) error {
	for _, info := range conf.Infos {
		if slices.Contains(effective, info.Name) {
			continue
		}
		return fmt.Errorf("the composition root provides the workspace configuration group %q run-wide, but this resolution did not plan for it: its ${endpoint:…} references were resolved against no producer mappings, so a value of it may be silently absent. The world was built without a composition-root group source (configurations.Loader.CompositionRootWorkspaceConfigurationNames)", info.Name)
	}
	return nil
}

// referencingWorkspaceConfigurationValues is every (group, key) of the effective
// set whose value carries an ${endpoint:…}, read from the configurations as they
// were LOADED — before interpolation, the only form in which a reference is
// still visible. The references are core's own extraction
// (resources.ConfigurationValueEndpointReferences), which also reads an assembly
// template's literals: a reference written there is invisible to .Value, and
// missing one is exactly the silent pass this exists to remove.
func (world *World) referencingWorkspaceConfigurationValues(groups []string) map[string][]string {
	if world == nil || world.providedWorkspaceConfigurationInfos == nil {
		return nil
	}
	wanted := make(map[string]bool, len(groups))
	for _, group := range groups {
		wanted[group] = true
	}
	out := map[string][]string{}
	for _, info := range world.providedWorkspaceConfigurationInfos() {
		if !wanted[info.GetName()] {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			references := resources.ConfigurationValueEndpointReferences(value)
			if len(references) == 0 {
				continue
			}
			out[info.GetName()+"/"+value.GetKey()] = references
		}
	}
	return out
}

// refuseDroppedWorkspaceConfigurationValues compares what the resolution was
// asked to deliver with what it actually delivered, and decides what a missing
// value means.
//
// It observes the outcome rather than predicting it, and that is the whole
// point. The earlier revision asked "do the bound mappings carry an endpoint
// with this identity?" and treated yes as resolution — so a mapping with no
// instance for the consumer's network access, an instance with an empty
// address, and a reference missing its endpoint component all read as resolved
// while core dropped the value and returned no error. Three shapes, one cause:
// a local approximation of whether core's interpolation would succeed. Core's
// interpolation is the only thing that knows, so this asks it the only way an
// outside caller can — by looking at what came back.
//
// What a missing value means then depends on the producer, as before:
//
//   - its producer is not part of this run (excluded infrastructure, or a run of
//     fewer services than the workspace): a legitimate drop, no composition
//     change makes it resolvable. Dropped, with a WARN naming the consumer, the
//     producer and the reference — core's own drop for this case is a DEBUG line.
//   - a render, and the producer IS part of the deployment: refused. A deployed
//     address is a pure function of identity and namespace, so there is no "not
//     yet", and the alternative is a committed manifest missing a value that
//     stays invisible until a client dials it.
//   - a local run, and the producer is part of the run: dropped with a WARN.
//     Narrower than it reads — with deterministic ports an early consumer still
//     resolves from the endpoints the producer recorded at Load — and reached
//     under --temporary-ports, where no address exists before the producer
//     initializes and nothing orders a root group's reference.
func (world *World) refuseDroppedWorkspaceConfigurationValues(
	ctx context.Context, service *resources.Service, groups []string, delivered []*basev0.Configuration,
	withheld map[string]bool,
) error {
	if world == nil || len(groups) == 0 {
		return nil
	}
	referencing := world.referencingWorkspaceConfigurationValues(groups)
	if len(referencing) == 0 {
		return nil
	}
	present := make(map[string]bool)
	for _, conf := range delivered {
		for _, info := range conf.GetInfos() {
			for _, value := range info.GetConfigurationValues() {
				present[info.GetName()+"/"+value.GetKey()] = true
			}
		}
	}
	consumer := consumerLabel(service)
	for _, qualified := range slices.Sorted(maps.Keys(referencing)) {
		if present[qualified] || withheld[qualified] {
			continue
		}
		if err := world.judgeDroppedValue(ctx, consumer, qualified, referencing[qualified]); err != nil {
			return err
		}
	}
	return nil
}

// judgeDroppedValue decides what one missing value means, per reference, and
// refuses on the first reference that must not be lost.
//
// The two paths ask different questions, and conflating them is what made the
// render's refusal unreachable in practice:
//
//   - A RENDER asks whether the producer is a service of the WORKSPACE. A
//     deployed address is a pure function of identity and namespace, so it is
//     derivable for any service the workspace has, whether or not this render's
//     flow covers it — and a render flow covers one root service's build closure
//     (Flow.managerDependencies → Dependencies.Restrict), which a root group's
//     reference adds no edge to. Judging a render by run membership therefore
//     exempted exactly the #882 case: another module's producer, named by a root
//     group, never in this flow's run set, value quietly absent from the
//     manifest. There is no excused drop left here: a producer the workspace does
//     not have was the last one, and it is refused before any outcome is
//     examined (checkEffectiveWorkspaceConfigurationReferences propagates core's
//     verdict), so the branch below exists only to refuse rather than to warn if
//     that ever changes.
//   - A RUN asks whether the producer is part of the run, because a local
//     address exists only for a service of this run. Both answers are drops: a
//     producer outside the run could never resolve here, and one inside it may
//     simply have no address yet (--temporary-ports, or no recorded endpoint).
//     Refusing either would fail a working local run.
//
// Every reference of the value is judged, not just the first: a value naming two
// producers, one outside the run and one that failed inside it, would otherwise
// be written off as a legitimate drop and hide the real fault.
func (world *World) judgeDroppedValue(ctx context.Context, consumer, qualified string, references []string) error {
	inRun := world.producerInRun()
	total := len(references)
	for index, reference := range references {
		// A reference is named by its POSITION in the value, never by its text.
		// A reference is text FROM a value and a value may be a secret, so a
		// diagnostic that reconstructs one — even one whose every token
		// validated — publishes part of the value it is reporting on. The
		// group, the key and which reference of that value it is locate it in
		// the operator's own file, which is where the text already is.
		position := fmt.Sprintf("reference %d of %d", index+1, total)
		info, err := resources.ParseEndpoint(reference)
		if err != nil {
			// Unreachable, and an error rather than a skip for that reason.
			// checkEffectiveWorkspaceConfigurationReferences has already
			// propagated core's verdict on every reference of the effective
			// set, and core calls a reference it cannot parse malformed. If
			// that ever stops being true, a reference nothing could read must
			// not become a value nothing delivers in silence.
			return fmt.Errorf("the workspace configuration value %s carries a reference that cannot be read (%s), and the value is missing for %s: %w",
				qualified, position, consumer, err)
		}
		producer := info.Module + "/" + info.Service
		if world.deploys() {
			if !world.workspaceHasProducer(ctx, producer) {
				// Unreachable for the same reason: core's verdict on a producer
				// the workspace does not have ("the producer is not a service
				// of this workspace") is propagated before any outcome is
				// examined, so this case never arrives here. It used to be the
				// render's one excused drop; it is an error now, because
				// excusing it is only correct while another guard refuses it,
				// and the reason it is unreachable is precisely that that guard
				// is here rather than only at the plan gate.
				return fmt.Errorf("the workspace configuration value %s is missing for %s: its %s names a producer that is not a service of this workspace, and the reference check did not refuse it — a render must not emit a manifest whose value is silently absent",
					qualified, consumer, position)
			}
			return fmt.Errorf("the workspace configuration value %s was dropped for %s: its %s names a service of this workspace whose deployed address is a function of its identity and namespace, so the value did not survive resolution for a reason the render cannot excuse — the manifest would simply lack it, which stays invisible until a client dials it",
				qualified, consumer, position)
		}
		if inRun == nil {
			// Nothing is provably in the run, so no missing value is provably a
			// fault — the same basis core requires before it may drop.
			continue
		}
		if !inRun(producer) {
			warnDroppedWorkspaceConfigurationReference(ctx, consumer, qualified, position,
				"its producer is not part of this run (excluded, or a run of fewer services than the workspace), so no local address exists for it")
			continue
		}
		warnDroppedWorkspaceConfigurationReference(ctx, consumer, qualified, position,
			"its producer is part of this run and no address could be derived for it yet (it recorded no endpoint, or --temporary-ports allocates its address at initialization)")
	}
	return nil
}

// workspaceHasProducer reports whether <module>/<service> is a service of the
// workspace. A world with no usable lookup answers true: the render then refuses
// a dropped value rather than excusing it, which is the safe direction — a
// refusal an operator can read beats a manifest missing a value nobody sees.
func (world *World) workspaceHasProducer(ctx context.Context, producer string) bool {
	lookup, err := world.workspaceProducers(ctx)
	if err != nil || lookup == nil {
		return true
	}
	_, ok := lookup(producer)
	return ok
}

// warnDroppedWorkspaceConfigurationReference says, at WARN, that a value is
// about to be missing from what a service receives. The level is the point: core
// drops the same value at DEBUG, and a configuration value absent without a word
// is the fault cli#882 exists to remove.
//
// It names the consumer, the value and WHICH reference of it — never the
// reference's text. A log line is the easiest place for a value to leak: it is
// written whether or not anyone is watching, kept, and shipped. The group, the
// key and the position locate the reference in the operator's own file.
func warnDroppedWorkspaceConfigurationReference(ctx context.Context, consumer, qualified, position, because string) {
	wool.Get(ctx).In("World.refuseDroppedWorkspaceConfigurationValues").Warn(
		"a workspace configuration value is missing for this service: "+because,
		wool.Field("consumer", consumer),
		wool.Field("value", qualified), wool.Field("reference", position))
}

// consumerLabel names the service a diagnostic is about. resources.WithUnique
// panics on a service whose module was never set, which a diagnostic must not
// do: a partially built service is exactly the state a resolution fault is
// reported from.
func consumerLabel(service *resources.Service) string {
	identity, err := service.Identity()
	if err != nil {
		return service.Name
	}
	return identity.Unique()
}

// applyWorkspaceConfigurationValues layers the run's derived values onto the
// resolved configurations, for the groups this service actually declares. A
// derived value therefore arrives on the same CODEFLY__WORKSPACE_CONFIGURATION
// carrier a declared one does, which is the contract a service reads — not a
// raw process variable it would only see through an incidental os.Getenv
// fallback.
//
// A derived value replaces a declared one for the same key: these are values
// the run mints for this run (a per-run credential digest), so a stale
// declaration must not win and leave the run's two halves unable to match.
func (world *World) applyWorkspaceConfigurationValues(
	resolved []*basev0.Configuration, dependencies []string,
) []*basev0.Configuration {
	if len(world.workspaceConfigurationValues) == 0 {
		return resolved
	}
	for _, group := range dependencies {
		values := world.workspaceConfigurationValues[group]
		if len(values) == 0 {
			continue
		}
		resolved = upsertWorkspaceConfigurationValues(resolved, group, values)
	}
	return resolved
}

// upsertWorkspaceConfigurationValues sets values on the named group, adding the
// group (and the workspace-origin configuration carrying it) when the
// composition declared none.
func upsertWorkspaceConfigurationValues(
	resolved []*basev0.Configuration, group string, values map[string]string,
) []*basev0.Configuration {
	for _, conf := range resolved {
		if conf.Origin != resources.ConfigurationWorkspace {
			continue
		}
		for _, info := range conf.Infos {
			if info.Name != group {
				continue
			}
			setConfigurationValues(info, values)
			return resolved
		}
	}
	info := &basev0.ConfigurationInformation{Name: group}
	setConfigurationValues(info, values)
	return append(resolved, &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos:  []*basev0.ConfigurationInformation{info},
	})
}

func setConfigurationValues(info *basev0.ConfigurationInformation, values map[string]string) {
	for _, key := range slices.Sorted(maps.Keys(values)) {
		replaced := false
		for _, existing := range info.ConfigurationValues {
			if existing.Key == key {
				existing.Value = values[key]
				replaced = true
				break
			}
		}
		if !replaced {
			info.ConfigurationValues = append(info.ConfigurationValues,
				&basev0.ConfigurationValue{Key: key, Value: values[key]})
		}
	}
}

// workspaceConfigurationSeen reports whether every Info name in a resolved
// workspace configuration is already present in seen (all Infos of a workspace
// configuration share one name), i.e. the configuration was already emitted via
// the declared-dependency set.
func (world *World) workspaceConfigurationSeen(conf *basev0.Configuration, seen map[string]bool) bool {
	for _, info := range conf.Infos {
		if seen[info.Name] {
			return true
		}
	}
	return false
}

// workspaceConfigurationExcluded reports whether a resolved workspace
// configuration is profile-excluded. Workspace configurations carry their name
// on each Info (the Configuration.Origin is always "workspace"), and every Info
// in a given configuration shares that name.
func (world *World) workspaceConfigurationExcluded(conf *basev0.Configuration) bool {
	for _, info := range conf.Infos {
		if world.excludedWorkspaceConfigurations[info.Name] {
			return true
		}
	}
	return false
}

// workspaceConfigurations resolves the groups this running service receives, at
// the addresses matching its runtime context. The run path's only entry point
// into the resolution above.
func (runner *Runner) workspaceConfigurations(
	ctx context.Context, dependencyMappings []*basev0.NetworkMapping, runtimeContext *basev0.RuntimeContext,
) ([]*basev0.Configuration, error) {
	return runner.world.workspaceConfigurationsFor(ctx, runner.instance.Service,
		dependencyMappings, resources.NetworkAccessFromRuntimeContext(runtimeContext))
}

// workspaceConfigurations resolves the groups this deployed service receives, at
// the in-cluster addresses its dependencies and the producers its groups
// reference serve. The render path's only entry point into the resolution above:
// the set is the same one the run resolves, and only the address family differs.
func (b *Builder) workspaceConfigurations(
	ctx context.Context, dependencyMappings []*basev0.NetworkMapping,
) ([]*basev0.Configuration, error) {
	return b.world.workspaceConfigurationsFor(ctx, b.instance.Service,
		dependencyMappings, resources.NewContainerNetworkAccess())
}
