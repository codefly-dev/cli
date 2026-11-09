package gitops

// --- What one module's promotion may change in the cell file ---
//
// The cell file is every module's approved builds in one document, and the
// cell refuses a pod whose images are not exactly the ones the file approves.
// A render derives the file from every module's tree as it stands in the
// checkout, so after a render that rebuilt modules A and B the file approves
// A's new digests AND B's. Delivering that file whole with A's promotion
// approved B's new build while B's delivered tree still ran the old one: B's
// next pod was refused, and B stayed down until someone published it. That
// is the outage this file closes.
//
// A module's promotion therefore carries the delivery base's cell file with
// only that module's entries moved to the render's, and every other module's
// entries exactly as the base has them — the builds the base approves are the
// builds the base's trees run. What a publication of module M may change:
//
//   - M's namespaces: every namespace whose `module` is M, as the render
//     describes it (added, replaced or removed). Each namespace is the image
//     of one module's delivered tree — its workloads, digests, release,
//     egress, ingress and delivery Job are all rendered into M's tree — so it
//     moves with that tree and with nothing else. Except another caller's
//     edge on one of M's endpoints the base already carries: that is the
//     caller's statement, and stays the base's (below).
//   - M's caller edges: in another module's namespace, the consumers of an
//     endpoint that name M (`M/<service>`), as the render states them for an
//     endpoint it still describes. An edge is the caller's statement of what
//     it calls, so the module whose new build makes the call is the one that
//     delivers the edge; the endpoint itself, and every other caller's edge on
//     it, stay the base's.
//   - A module the cell withdraws: one the render no longer describes, that
//     the composition explains as removed (compositionClaim) or the
//     publication names with --allow-cell-removal — its namespaces and its
//     caller edges go. That is the one cross-module removal a publication
//     already makes, and the hold (holdCellToBase) reports it as it always
//     has.
//   - The source revision: the render this publication delivers. It is
//     provenance — the commit a later publication's checkout must contain to
//     withdraw on the composition's word — and every module of one render
//     states the same one, so it is deterministic: the last publication's
//     render. A render with no source (not a git checkout) keeps the base's.
//   - The cell-wide header (schema, coordinate, component, environment,
//     domain, trust domain, delivery target, and the source's repository,
//     branch and path): only by the publication of the module that serves the
//     delivery API, which is the host the header describes. Any other module's
//     publication carrying a different header is refused, naming the fields:
//     a header that moved under one module's promotion would re-scope every
//     module's approvals in the cell without any of them being delivered.
//
// Everything else is the base's, byte for byte. Publishing every module of a
// render, in any order, therefore delivers the render's whole file; publishing
// one delivers that module and leaves the rest of the cell as it was.
//
// A base with no cell file is the one exception: nothing is approved yet, so
// there is no other module's approval to preserve, and the first file is the
// render's own.

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
)

// errHeaderMoved is a render whose cell-wide header differs from the delivery
// base's, published by a module that does not carry the header.
var errHeaderMoved = errors.New("the render moves the cell-wide header")

// scopeAndHoldCell scopes a forward publication's cell file to the published
// module (scopeToModule), then holds the file that is carried to the base's
// (holdCellToBase). A forward publication carries the base's cell file with
// only this module's entries moved to the render's, never the render's file
// whole: every other module keeps the builds the base approves, which are the
// builds the base's trees run. forward is false when the publication carries
// no cell file; the hold then judges what there is, as before.
func scopeAndHoldCell(cell *publicationCell, forward bool, env *environments.Environment, baseBranch string, base *cellfile.File, request *PublishRequest, claim *compositionClaim) (named, undeclared []string, err error) {
	if forward {
		if err = cell.scopeToModule(env, base, request.Module, request.AllowCellRemovals, claim); err != nil {
			return nil, nil, err
		}
	}
	return holdCellToBase(cell, env, baseBranch, base, request.AllowCellRemovals, claim)
}

// scopeToModule replaces what the publication carries — the render's file —
// with what a promotion of module may deliver: the base's file with only the
// module's entries moved (promotedCell). The hold and the delivery then read
// the scoped file, so what is held is what is delivered.
func (cell *publicationCell) scopeToModule(env *environments.Environment, base *cellfile.File, module string, allowed []string, claim *compositionClaim) error {
	if cell == nil || base == nil || env == nil || env.Host == nil {
		return nil
	}
	withdraws := func(other string) bool {
		return slices.Contains(allowed, other) || (claim != nil && claim.explains(other))
	}
	hostModule, _, _ := env.Host.DeliveryEndpoint()
	scoped, err := promotedCell(base, cell.file, module, withdraws, module == hostModule)
	if errors.Is(err, errHeaderMoved) {
		return fmt.Errorf("environment %s's cell file %s: %w: the header describes the host, so it moves only with the publication of %s, which serves the delivery API; publish %s first, or render from a checkout whose environment matches the delivery base's",
			env.Name, cell.source, err, hostModule, hostModule)
	}
	if err != nil {
		return fmt.Errorf("environment %s's cell file %s: %w", env.Name, cell.source, err)
	}
	data, err := scoped.Encode()
	if err != nil {
		return fmt.Errorf("the cell file module %s's promotion carries cannot be written: %w", module, err)
	}
	cell.file, cell.data = scoped, data
	return nil
}

// promotedCell is the cell file a publication of module delivers onto base:
// base, with module's namespaces and caller edges taken from rendered, the
// modules withdraws names (and rendered no longer describes) removed, and
// the source revision rendered's. carriesHeader is whether module may move
// the cell-wide header; when it may not, a rendered header that differs from
// base's is refused. A nil base answers rendered: nothing is approved yet.
// Neither input is modified.
func promotedCell(base, rendered *cellfile.File, module string, withdraws func(string) bool, carriesHeader bool) (*cellfile.File, error) {
	if rendered == nil {
		return nil, fmt.Errorf("no rendered cell file to promote module %s from", module)
	}
	if base == nil {
		return rendered, nil
	}
	if !carriesHeader {
		if moved := headerChanges(base, rendered); len(moved) > 0 {
			return nil, fmt.Errorf("%w (%s), which a publication of module %s does not carry", errHeaderMoved, strings.Join(moved, "; "), module)
		}
	}
	described := map[string]bool{}
	for index := range rendered.Namespaces {
		described[rendered.Namespaces[index].Module] = true
	}
	gone := func(other string) bool {
		return other != module && !described[other] && withdraws != nil && withdraws(other)
	}

	next := *rendered
	next.Source = promotedSource(base.Source, rendered.Source)
	next.Namespaces = nil
	for index := range rendered.Namespaces {
		if namespace := &rendered.Namespaces[index]; namespace.Module == module {
			next.Namespaces = append(next.Namespaces, withOthersEdges(namespace, baseNamespace(base, namespace), module, gone))
		}
	}
	for index := range base.Namespaces {
		namespace := &base.Namespaces[index]
		if namespace.Module == module || gone(namespace.Module) {
			continue
		}
		if slices.ContainsFunc(next.Namespaces, func(candidate cellfile.Namespace) bool { return candidate.Name == namespace.Name }) {
			return nil, fmt.Errorf("module %s's render names namespace %s, which the delivery base's cell file gives module %s", module, namespace.Name, namespace.Module)
		}
		next.Namespaces = append(next.Namespaces, withCallerEdges(namespace, renderedNamespace(rendered, namespace), module, gone))
	}
	sort.Slice(next.Namespaces, func(i, j int) bool { return next.Namespaces[i].Name < next.Namespaces[j].Name })
	return &next, nil
}

// promotedSource is the source the promoted file names: the render's, which
// is the render being delivered, or the base's when the render could name
// none. A copy either way, so the result shares nothing with its inputs.
func promotedSource(base, rendered *cellfile.Source) *cellfile.Source {
	source := rendered
	if source == nil {
		source = base
	}
	if source == nil {
		return nil
	}
	copied := *source
	return &copied
}

// headerChanges names every cell-wide field rendered states differently from
// base. The source's revision is not one: it is the render's provenance, not
// a property of the cell. The source's repository, branch and path are: they
// say where every module's trees are delivered from. A render with no source
// states none of them, and changes none.
func headerChanges(base, rendered *cellfile.File) []string {
	var moved []string
	field := func(name, was, is string) {
		if was != is {
			moved = append(moved, fmt.Sprintf("%s %q → %q", name, was, is))
		}
	}
	field("schema", base.Schema, rendered.Schema)
	field("coordinate", base.Coordinate, rendered.Coordinate)
	field("component", base.Component, rendered.Component)
	field("environment", base.Environment, rendered.Environment)
	field("domain", base.Domain, rendered.Domain)
	field("trust_domain", base.TrustDomain, rendered.TrustDomain)
	field("delivery_target", base.DeliveryTarget, rendered.DeliveryTarget)
	if base.Source != nil && rendered.Source != nil {
		field("source.repo", base.Source.Repo, rendered.Source.Repo)
		field("source.branch", base.Source.Branch, rendered.Source.Branch)
		field("source.path", base.Source.Path, rendered.Source.Path)
	}
	return moved
}

// baseNamespace is the base's namespace of the same name given to the same
// module as namespace, or nil when the base has none.
func baseNamespace(base *cellfile.File, namespace *cellfile.Namespace) *cellfile.Namespace {
	return renderedNamespace(base, namespace)
}

// withOthersEdges is module's own namespace as the render describes it, with
// every other caller's edge on an endpoint the base already carries kept as
// the base has it. An edge is the caller's statement, delivered by the
// caller's publication: a render of the provider predating a caller's
// withdrawal of its edge must not restore that edge (a network permission the
// caller's delivered declaration no longer asks for), and one stating a
// caller's new edge must not deliver it before the caller's build is. An
// endpoint the base does not carry is new with this promotion, so the render's
// list is all there is. Only consumer lists are rebuilt, as fresh slices.
func withOthersEdges(rendered, base *cellfile.Namespace, module string, gone func(string) bool) cellfile.Namespace {
	own := *rendered
	if base == nil {
		return own
	}
	own.Workloads = slices.Clone(rendered.Workloads)
	for w := range own.Workloads {
		workload := &own.Workloads[w]
		found := slices.IndexFunc(base.Workloads, func(candidate cellfile.Workload) bool {
			return candidate.Name == workload.Name && candidate.Kind == workload.Kind
		})
		if found < 0 {
			continue
		}
		baseWorkload := &base.Workloads[found]
		workload.Endpoints = slices.Clone(workload.Endpoints)
		for e := range workload.Endpoints {
			endpoint := &workload.Endpoints[e]
			if at := slices.IndexFunc(baseWorkload.Endpoints, func(candidate cellfile.Endpoint) bool { return candidate.Name == endpoint.Name }); at >= 0 {
				endpoint.Consumers = callerEdges(baseWorkload.Endpoints[at].Consumers, endpoint.Consumers, module, gone)
			}
		}
	}
	return own
}

// renderedNamespace is the render's description of base's namespace — the
// same name, given to the same module — or nil when the render has none.
func renderedNamespace(rendered *cellfile.File, namespace *cellfile.Namespace) *cellfile.Namespace {
	for index := range rendered.Namespaces {
		if candidate := &rendered.Namespaces[index]; candidate.Name == namespace.Name && candidate.Module == namespace.Module {
			return candidate
		}
	}
	return nil
}

// withCallerEdges is another module's namespace as the base carries it, with
// module's caller edges on each endpoint taken from the render's description
// of that endpoint, and every edge of a withdrawn module dropped. Where the
// render does not describe the endpoint, module's edges on it stay the base's:
// the render says nothing about them. Only the consumer lists are rebuilt, as
// fresh slices, so base is not modified.
func withCallerEdges(namespace *cellfile.Namespace, rendered *cellfile.Namespace, module string, gone func(string) bool) cellfile.Namespace {
	kept := *namespace
	kept.Workloads = slices.Clone(namespace.Workloads)
	for w := range kept.Workloads {
		workload := &kept.Workloads[w]
		var renderedWorkload *cellfile.Workload
		if rendered != nil {
			if found := slices.IndexFunc(rendered.Workloads, func(candidate cellfile.Workload) bool {
				return candidate.Name == workload.Name && candidate.Kind == workload.Kind
			}); found >= 0 {
				renderedWorkload = &rendered.Workloads[found]
			}
		}
		workload.Endpoints = slices.Clone(workload.Endpoints)
		for e := range workload.Endpoints {
			endpoint := &workload.Endpoints[e]
			stated := endpoint.Consumers
			if renderedWorkload != nil {
				if found := slices.IndexFunc(renderedWorkload.Endpoints, func(candidate cellfile.Endpoint) bool { return candidate.Name == endpoint.Name }); found >= 0 {
					stated = renderedWorkload.Endpoints[found].Consumers
				}
			}
			endpoint.Consumers = callerEdges(endpoint.Consumers, stated, module, gone)
		}
	}
	return kept
}

// callerEdges is one endpoint's consumer list after module's promotion: the
// base's list, in the base's order — the render does not sort it, and an
// order the promotion changed would be a change to every other caller's
// lines — less module's edges the render no longer states and every
// withdrawn module's, with module's newly stated edges appended in the
// render's order. A list the promotion does not change is the base's own.
func callerEdges(base, stated []string, module string, gone func(string) bool) []string {
	mine := func(consumer string) bool { return consumerModule(consumer) == module }
	var consumers []string
	for _, consumer := range base {
		switch caller := consumerModule(consumer); {
		case caller == module && !slices.Contains(stated, consumer), caller != module && gone(caller):
		default:
			consumers = append(consumers, consumer)
		}
	}
	for _, consumer := range stated {
		if mine(consumer) && !slices.Contains(consumers, consumer) {
			consumers = append(consumers, consumer)
		}
	}
	if slices.Equal(consumers, base) {
		return base
	}
	return consumers
}

// consumerModule is the module of a consumer edge, `<module>/<service>`.
func consumerModule(consumer string) string {
	module, _, _ := strings.Cut(consumer, "/")
	return module
}
