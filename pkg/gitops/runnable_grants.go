package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/solutionhost"
)

// --- What a module that installs prepared bindings is granted ---
//
// A service that declares the runnable-bindings workspace configuration group
// installs every binding the composition prepared, and calls each owner with
// a capability exchanged for the call (derivedRunnableReach). The mesh already
// admits it to those owners from that one derivation; this is the same
// derivation read for authority: the module's request names each prepared
// binding it installs, so the host has a binding to exchange through.
//
// A binding is named by its prepared key (MODULE__OPERATION), the one id the
// installer reads the value under, and carries the audience and the resource
// kinds and actions the binding's resolved policy states. It never carries a
// resource id: which resources an organization lets the module reach is that
// organization's consent, written to the module's installation scope, which
// narrows the binding there.
//
// Only a module that asks for authority gains these bindings. A module that
// publishes no request asks for nothing, and the host grants it its envelope
// whole; adding a request would change what it is granted, so its envelope
// stays the composition's to state.

// preparedGrant is what one prepared binding asks for, without resource ids.
type preparedGrant struct {
	key      string
	audience string
	invoke   []preparedScope
	lookup   []preparedScope
}

// preparedScope is one resource kind and the actions a binding asks for on it.
type preparedScope struct {
	kind    string
	actions []string
}

// installsPreparedBindings reports whether any of a module's services declares
// the runnable-bindings group, which is what installing prepared bindings is.
func installsPreparedBindings(services []*resources.Service) bool {
	for _, service := range services {
		if service != nil && slices.Contains(service.WorkspaceConfigurationDependencies, runnablespkg.RunnableBindingsGroup) {
			return true
		}
	}
	return false
}

// preparedGrants reads every binding the composition prepared for the
// environment, sorted by key. A value that is not a prepared binding fails the
// read by key: it is generated output, and one that does not decode would
// install nothing.
func preparedGrants(provided *configurations.WorkspaceConfigurations) ([]preparedGrant, error) {
	if provided == nil {
		return nil, nil
	}
	var grants []preparedGrant
	for _, info := range provided.Infos {
		if info.GetName() != runnablespkg.RunnableBindingsGroup {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			prepared, err := corerunnable.DecodePrepared([]byte(value.GetValue()))
			if err != nil {
				return nil, fmt.Errorf("%s/%s is not a prepared binding; regenerate it with `codefly generate runnable-bindings`: %w", runnablespkg.RunnableBindingsGroup, value.GetKey(), err)
			}
			policy := prepared.GetPolicy()
			grants = append(grants, preparedGrant{
				key:      value.GetKey(),
				audience: policy.GetAudience(),
				invoke:   kindScopes(policy.GetInvokeScopes()),
				lookup:   kindScopes(policy.GetLookupScopes()),
			})
		}
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].key < grants[j].key })
	for i := 1; i < len(grants); i++ {
		if grants[i].key == grants[i-1].key {
			return nil, fmt.Errorf("%s/%s is supplied twice; a prepared binding has one value", runnablespkg.RunnableBindingsGroup, grants[i].key)
		}
	}
	return grants, nil
}

// kindScopes keeps each scope's kind and actions and drops its resource ids,
// merging scopes over one kind, sorted by kind and action.
func kindScopes(scopes []*basev0.WorkScopeV1) []preparedScope {
	byKind := map[string][]string{}
	for _, scope := range scopes {
		kind := scope.GetResourceKind()
		for _, action := range scope.GetActions() {
			if !slices.Contains(byKind[kind], action) {
				byKind[kind] = append(byKind[kind], action)
			}
		}
	}
	out := make([]preparedScope, 0, len(byKind))
	for kind, actions := range byKind {
		sort.Strings(actions)
		out = append(out, preparedScope{kind: kind, actions: actions})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].kind < out[j].kind })
	return out
}

// requestBinding is the binding the module's request asks for.
func (g *preparedGrant) requestBinding() solutionhost.AuthorityBinding {
	operations := map[string][]string{modulecontract.OperationInvoke: scopeStrings(g.invoke)}
	if len(g.lookup) > 0 {
		operations[modulecontract.OperationLookup] = scopeStrings(g.lookup)
	}
	return solutionhost.AuthorityBinding{ID: g.key, Audience: g.audience, Operations: operations}
}

func scopeStrings(scopes []preparedScope) []string {
	var out []string
	for _, scope := range scopes {
		for _, action := range scope.actions {
			out = append(out, scope.kind+":"+action)
		}
	}
	sort.Strings(out)
	return out
}

// appendPreparedBindings adds a binding per prepared key to a module's request.
// A key the module's own contract already uses is refused by name: one id, two
// declarations of what it asks.
//
// A prepared binding addressed to the module itself is not installed: the
// composition prepares every runnable for every installer, so a module whose
// own operations are runnables (robin's chat methods) would otherwise ask for
// a person-delegated capability to call itself — which the host refuses, as
// a binding naming its own module is only ever headless work — and reaching
// its own operations needs no capability at all.
func appendPreparedBindings(authority *solutionhost.Authority, module string, grants []preparedGrant) error {
	for i := range grants {
		grant := &grants[i]
		if grant.audience == module {
			continue
		}
		for _, existing := range authority.Bindings {
			if existing.ID == grant.key {
				return fmt.Errorf("module %s declares binding %s and installs the prepared binding %s/%s under the same id; a binding has one declaration", module, existing.ID, runnablespkg.RunnableBindingsGroup, grant.key)
			}
		}
		authority.Bindings = append(authority.Bindings, grant.requestBinding())
	}
	return nil
}

// --- The envelope entry: a labelled stopgap ---
//
// The host takes a binding's delegation mode only from the envelope's entry
// for it, so a requested binding the envelope does not list is never granted,
// and one listed without a mode exchanges a child that does not name the
// installing module as its actor. Until the mode travels with the request,
// the envelope therefore lists each prepared binding an asking module
// installs: the binding's audience, its kinds and actions WITHOUT resource ids
// — the installation scope narrows them per organization — and the
// module_actor mode, so a call through it is recorded as the installing
// module acting for the person. An entry the composition states for the same
// key is the composition's and is kept as stated.
//
// STOPGAP: this exists only because the host reads the mode from the envelope.
// When the mode moves to the manifest's binding, delete it.

// preparedBindingDelegationMode is the mode every generated entry carries.
const preparedBindingDelegationMode = "module_actor"

// envelopeScope is one scope of an envelope entry, in the host's grant shape.
type envelopeScope struct {
	ResourceKind string   `json:"resource_kind"`
	Actions      []string `json:"actions"`
}

// envelopeEntry is one operation audience of an envelope grant, in the host's
// grant shape.
type envelopeEntry struct {
	DelegationMode string          `json:"delegation_mode"`
	Audience       string          `json:"audience"`
	InvokeScopes   []envelopeScope `json:"invoke_scopes"`
	LookupScopes   []envelopeScope `json:"lookup_scopes,omitempty"`
}

func (g *preparedGrant) envelopeEntry() envelopeEntry {
	entry := envelopeEntry{DelegationMode: preparedBindingDelegationMode, Audience: g.audience}
	for _, scope := range g.invoke {
		entry.InvokeScopes = append(entry.InvokeScopes, envelopeScope{ResourceKind: scope.kind, Actions: slices.Clone(scope.actions)})
	}
	for _, scope := range g.lookup {
		entry.LookupScopes = append(entry.LookupScopes, envelopeScope{ResourceKind: scope.kind, Actions: slices.Clone(scope.actions)})
	}
	return entry
}

// WithPreparedBindingGrants returns the composition's envelope grants with an
// entry for each prepared binding a module installs, for every module that
// asks for authority and whose principal the composition grants. A
// composition that grants nothing is returned unchanged: an envelope nobody
// states is not one this writes.
func WithPreparedBindingGrants(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, principals map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(principals) == 0 || workspace == nil || env == nil {
		return principals, nil
	}
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}
	var installers []string
	for _, module := range modules {
		if _, granted := principals[module.Name]; !granted {
			continue
		}
		services, loadErr := loadModuleServices(ctx, module, env)
		if loadErr != nil {
			return nil, loadErr
		}
		if !installsPreparedBindings(services) {
			continue
		}
		request, requestErr := authorityRequestOf(module)
		if requestErr != nil {
			return nil, requestErr
		}
		if request.undeclared != "" {
			continue
		}
		installers = append(installers, module.Name)
	}
	if len(installers) == 0 {
		return principals, nil
	}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime(), orchestration.ModuleArtifactResolverFor(workspace))
	if err != nil {
		return nil, fmt.Errorf("read the prepared bindings the envelope lists: %w", err)
	}
	grants, err := preparedGrants(provided)
	if err != nil || len(grants) == 0 {
		return principals, err
	}
	out := make(map[string]json.RawMessage, len(principals))
	for principal, grant := range principals {
		out[principal] = grant
	}
	for _, principal := range installers {
		merged, err := withPreparedEntries(principal, out[principal], grants)
		if err != nil {
			return nil, err
		}
		out[principal] = merged
	}
	return out, nil
}

// withPreparedEntries adds the generated entries to one principal's grant,
// keeping every entry the grant already states.
func withPreparedEntries(principal string, grant json.RawMessage, grants []preparedGrant) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if trimmed := strings.TrimSpace(string(grant)); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(grant, &fields); err != nil {
			return nil, fmt.Errorf("the envelope grant of %s is not an object: %w", principal, err)
		}
	}
	audiences := map[string]json.RawMessage{}
	if raw, ok := fields["operation_audiences"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &audiences); err != nil {
			return nil, fmt.Errorf("the envelope grant of %s states operation_audiences that are not an object: %w", principal, err)
		}
	}
	for i := range grants {
		prepared := &grants[i]
		if prepared.audience == principal {
			continue
		}
		if _, stated := audiences[prepared.key]; stated {
			continue
		}
		entry, err := json.Marshal(prepared.envelopeEntry())
		if err != nil {
			return nil, err
		}
		audiences[prepared.key] = entry
	}
	encoded, err := json.Marshal(audiences)
	if err != nil {
		return nil, err
	}
	fields["operation_audiences"] = encoded
	return json.Marshal(fields)
}
