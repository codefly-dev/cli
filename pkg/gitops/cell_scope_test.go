package gitops

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
)

func digest(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }

// scopeCell is a two-module cell in the shape a render writes: module a and
// module b, each one workload running one image, b serving an endpoint that
// both a and a third module, c, call.
func scopeCell(revision, aDigest, bDigest string, bConsumers ...string) *cellfile.File {
	workload := func(module, name, image string, endpoints []cellfile.Endpoint) cellfile.Workload {
		return cellfile.Workload{
			Name: name, Kind: "Deployment", Selector: map[string]string{"app": name},
			Service: module + "/" + name, ServiceAccount: name, Authenticating: name,
			SPIFFEID:   "spiffe://cluster.example/ns/" + module + "/sa/" + name,
			Containers: []cellfile.Container{{Name: name, Image: cellfile.Image{Repository: "registry.example/" + module, Digest: image}}},
			Release:    &cellfile.Release{Publisher: "example", Name: module, Version: image[7:15]},
			Endpoints:  endpoints,
		}
	}
	return &cellfile.File{
		Schema: cellfile.Schema, Coordinate: "example/blue/region-a", Component: "platform-host",
		Environment: "blue", Domain: "example", TrustDomain: "cluster.example", DeliveryTarget: "a/api/rest",
		Source: &cellfile.Source{Repo: "https://example.test/delivery.git", Revision: revision, Branch: "main"},
		Namespaces: []cellfile.Namespace{
			{Name: "a", Module: "a", Workloads: []cellfile.Workload{workload("a", "api", aDigest, nil)}},
			{Name: "b", Module: "b", Workloads: []cellfile.Workload{workload("b", "worker", bDigest, []cellfile.Endpoint{{Name: "grpc", API: "grpc", Port: 9090, Consumers: bConsumers}})}},
		},
	}
}

func encodeCell(t *testing.T, file *cellfile.File) []byte {
	t.Helper()
	data, err := file.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func promote(t *testing.T, base, rendered *cellfile.File, module string) *cellfile.File {
	t.Helper()
	next, err := promotedCell(base, rendered, module, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if outside := cellChangesOutside(base, next, module); len(outside) > 0 {
		t.Fatalf("the promotion of %s changes what is not its own: %v", module, outside)
	}
	return next
}

func namespaceDigest(t *testing.T, file *cellfile.File, module string) string {
	t.Helper()
	for _, namespace := range file.Namespaces {
		if namespace.Module == module {
			return namespace.Workloads[0].Containers[0].Image.Digest
		}
	}
	t.Fatalf("the cell names no namespace of %s", module)
	return ""
}

// cellChangesOutside is the guard every promotion is held to: what next
// changes against base that is not module's own. Module's own is its
// namespaces and its caller edges anywhere; the source revision is the
// render's provenance. Everything else — every other module's namespace,
// every other caller's edge, the cell-wide header — must be the base's.
func cellChangesOutside(base, next *cellfile.File, module string) []string {
	var outside []string
	header := func(file *cellfile.File) cellfile.File {
		copied := *file
		copied.Namespaces = nil
		if copied.Source != nil {
			source := *copied.Source
			source.Revision = ""
			copied.Source = &source
		}
		return copied
	}
	if was, is := header(base), header(next); !reflect.DeepEqual(was, is) {
		outside = append(outside, fmt.Sprintf("the header: %+v → %+v", was, is))
	}
	// Another module's namespace without module's caller edges: what the
	// promotion of module must leave exactly as it was.
	others := func(file *cellfile.File) map[string]string {
		described := map[string]string{}
		for _, namespace := range file.Namespaces {
			if namespace.Module == module {
				continue
			}
			// A deep copy through the file's own encoding, so stripping the
			// edges touches neither input — and shares no code with the
			// promotion it guards.
			alone, err := (&cellfile.File{Schema: cellfile.Schema, Namespaces: []cellfile.Namespace{namespace}}).Encode()
			if err != nil {
				described[namespace.Name] = err.Error()
				continue
			}
			copied, err := cellfile.Parse(alone)
			if err != nil {
				described[namespace.Name] = err.Error()
				continue
			}
			for w := range copied.Namespaces[0].Workloads {
				endpoints := copied.Namespaces[0].Workloads[w].Endpoints
				for e := range endpoints {
					endpoints[e].Consumers = slices.DeleteFunc(endpoints[e].Consumers, func(consumer string) bool {
						return strings.HasPrefix(consumer, module+"/")
					})
					if len(endpoints[e].Consumers) == 0 {
						endpoints[e].Consumers = nil
					}
				}
			}
			data, err := copied.Encode()
			if err != nil {
				described[namespace.Name] = err.Error()
				continue
			}
			described[namespace.Name] = string(data)
		}
		return described
	}
	// On module's own endpoints the base already carries, every other
	// caller's edge is that caller's statement, and must be the base's.
	othersOn := func(file *cellfile.File) map[string][]string {
		edges := map[string][]string{}
		for _, namespace := range file.Namespaces {
			if namespace.Module != module {
				continue
			}
			for _, workload := range namespace.Workloads {
				for _, endpoint := range workload.Endpoints {
					key := namespace.Name + "/" + workload.Kind + "/" + workload.Name + "/" + endpoint.Name
					edges[key] = slices.DeleteFunc(slices.Clone(endpoint.Consumers), func(consumer string) bool {
						return strings.HasPrefix(consumer, module+"/")
					})
				}
			}
		}
		return edges
	}
	wasOn, isOn := othersOn(base), othersOn(next)
	for key, edges := range isOn {
		if before, carried := wasOn[key]; carried && !slices.Equal(before, edges) {
			outside = append(outside, fmt.Sprintf("other callers on %s: %v → %v", key, before, edges))
		}
	}
	was, is := others(base), others(next)
	for name, body := range was {
		if is[name] != body {
			outside = append(outside, "namespace "+name)
		}
	}
	for name := range is {
		if _, found := was[name]; !found {
			outside = append(outside, "namespace "+name+" (added)")
		}
	}
	slices.Sort(outside)
	return outside
}

// TestPublishingAModuleMovesOnlyItsOwnApproval is the outage: a render rebuilt
// a and b, a was published, and its promotion approved b's new digest while
// b's delivered tree still ran the old one — b's next pod was refused. a's
// promotion now moves a's approval and leaves b's exactly as the base has it;
// b's own promotion moves b's; and once both are published, in either order,
// the cell carries the render's file byte for byte.
func TestPublishingAModuleMovesOnlyItsOwnApproval(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"))
	rendered := scopeCell(strings.Repeat("2", 40), digest("c"), digest("d"))
	baseBytes := encodeCell(t, base)

	afterA := promote(t, base, rendered, "a")
	if got := namespaceDigest(t, afterA, "a"); got != digest("c") {
		t.Fatalf("a's promotion approves a at %s, want its new build %s", got, digest("c"))
	}
	if got := namespaceDigest(t, afterA, "b"); got != digest("b") {
		t.Fatalf("a's promotion moved b's approval to %s; b's delivered tree still runs %s", got, digest("b"))
	}
	if !bytes.Equal(encodeCell(t, base), baseBytes) {
		t.Fatal("the promotion modified the base it was given")
	}

	afterB := promote(t, afterA, rendered, "b")
	if got := namespaceDigest(t, afterB, "b"); got != digest("d") {
		t.Fatalf("b's promotion approves b at %s, want its new build %s", got, digest("d"))
	}
	if got := namespaceDigest(t, afterB, "a"); got != digest("c") {
		t.Fatalf("b's promotion moved a's approval back to %s", got)
	}

	whole := encodeCell(t, rendered)
	if !bytes.Equal(encodeCell(t, afterB), whole) {
		t.Fatalf("publishing every module does not deliver the render's file:\n%s\nwant:\n%s", encodeCell(t, afterB), whole)
	}
	reversed := promote(t, promote(t, base, rendered, "b"), rendered, "a")
	if !bytes.Equal(encodeCell(t, reversed), whole) {
		t.Fatalf("publishing every module in the other order does not deliver the render's file:\n%s", encodeCell(t, reversed))
	}
}

// TestTheRevisionLineIsTheDeliveredRender: the source revision is the one
// cross-module line every promotion moves, and it moves one way — to the
// render the promotion delivers — so two modules of one render agree on it,
// a re-promotion is byte-identical, and a render that names no source keeps
// the base's rather than erasing it.
func TestTheRevisionLineIsTheDeliveredRender(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"))
	rendered := scopeCell(strings.Repeat("2", 40), digest("c"), digest("d"))

	afterA := promote(t, base, rendered, "a")
	if afterA.Source.Revision != rendered.Source.Revision {
		t.Fatalf("the promotion names revision %s, want the render's %s", afterA.Source.Revision, rendered.Source.Revision)
	}
	if again := promote(t, base, rendered, "a"); !bytes.Equal(encodeCell(t, again), encodeCell(t, afterA)) {
		t.Fatal("the same promotion of the same render is not byte-identical")
	}
	afterB := promote(t, base, rendered, "b")
	if afterB.Source.Revision != afterA.Source.Revision {
		t.Fatalf("two modules of one render name revisions %s and %s", afterA.Source.Revision, afterB.Source.Revision)
	}

	unsourced := scopeCell("", digest("c"), digest("d"))
	unsourced.Source = nil
	kept := promote(t, base, unsourced, "a")
	if kept.Source == nil || *kept.Source != *base.Source || kept.Source == base.Source {
		t.Fatalf("a render with no source changed the base's: %+v", kept.Source)
	}
}

// TestOnlyTheHostMovesTheCellHeader: the header describes the host — its
// coordinate, trust domain, delivery API — and every module's approvals are
// read under it, so a render whose header moved is refused for any module but
// the one serving the delivery API, naming what moved; that module carries it.
func TestOnlyTheHostMovesTheCellHeader(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"))
	rendered := scopeCell(strings.Repeat("2", 40), digest("c"), digest("d"))
	rendered.TrustDomain = "cluster.other"

	_, err := promotedCell(base, rendered, "b", nil, false)
	if err == nil || !strings.Contains(err.Error(), `trust_domain "cluster.example" → "cluster.other"`) {
		t.Fatalf("a module that is not the host moved the header: %v", err)
	}
	carried, err := promotedCell(base, rendered, "a", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if carried.TrustDomain != "cluster.other" || namespaceDigest(t, carried, "b") != digest("b") {
		t.Fatalf("the host's promotion did not carry the header alone: %+v", carried)
	}
}

// TestACallerEdgeMovesWithTheCaller: a consumer edge is the caller's
// statement of what it calls, so a's promotion delivers a's edges on b's
// endpoint as the render states them, and leaves every other caller's edge —
// c's new one included — as the base has it, for c's promotion to deliver.
func TestACallerEdgeMovesWithTheCaller(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"), "c/old")
	rendered := scopeCell(strings.Repeat("2", 40), digest("a"), digest("b"), "a/api", "c/new", "c/old")

	afterA := promote(t, base, rendered, "a")
	if got := afterA.Namespaces[1].Workloads[0].Endpoints[0].Consumers; !slices.Equal(got, []string{"c/old", "a/api"}) {
		t.Fatalf("a's promotion leaves b's endpoint called by %v, want c's as the base has it and a's edge added", got)
	}
	if got := base.Namespaces[1].Workloads[0].Endpoints[0].Consumers; !slices.Equal(got, []string{"c/old"}) {
		t.Fatalf("the promotion modified the base's edges: %v", got)
	}

	// a stops calling b: its edge goes, and that is a removal the hold judges.
	dropped := scopeCell(strings.Repeat("3", 40), digest("a"), digest("b"), "c/old")
	afterDrop := promote(t, afterA, dropped, "a")
	if got := afterDrop.Namespaces[1].Workloads[0].Endpoints[0].Consumers; !slices.Equal(got, []string{"c/old"}) {
		t.Fatalf("a's edge outlived a's render: %v", got)
	}
	if removals := cellRemovals(afterA, afterDrop); len(removals) != 1 || !slices.Equal(removals[0].modules, []string{"b", "a"}) {
		t.Fatalf("the dropped edge is not the one removal the hold sees: %+v", removals)
	}
}

// TestAProviderPromotionKeepsEveryOtherCallersEdge is the provider side of
// an edge: a stopped calling b and was published, so the delivery base no
// longer carries a's edge on b's endpoint. Publishing b from a render that
// predates a's change must not put a's edge back — a network permission a's
// delivered declaration no longer asks for, and an addition the removal hold
// never sees. Nor does b's promotion deliver c's new edge before c's own. An
// endpoint b adds with this promotion carries its edges as rendered.
func TestAProviderPromotionKeepsEveryOtherCallersEdge(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"), "c/old")
	stale := scopeCell(strings.Repeat("2", 40), digest("a"), digest("d"), "a/api", "c/old", "c/new", "b/self")

	afterB := promote(t, base, stale, "b")
	if got := afterB.Namespaces[1].Workloads[0].Endpoints[0].Consumers; !slices.Equal(got, []string{"c/old", "b/self"}) {
		t.Fatalf("b's promotion leaves its endpoint called by %v, want every other caller's edge as the base has it and b's own as rendered", got)
	}
	if got := namespaceDigest(t, afterB, "b"); got != digest("d") {
		t.Fatalf("b's promotion approves b at %s, want its new build", got)
	}
	if removals := cellRemovals(base, afterB); len(removals) != 0 {
		t.Fatalf("keeping the base's edges is no removal: %+v", removals)
	}

	added := scopeCell(strings.Repeat("3", 40), digest("a"), digest("d"), "c/old")
	added.Namespaces[1].Workloads[0].Endpoints = append(added.Namespaces[1].Workloads[0].Endpoints,
		cellfile.Endpoint{Name: "http", API: "http", Port: 8080, Consumers: []string{"a/api"}})
	withNew := promote(t, base, added, "b")
	if got := withNew.Namespaces[1].Workloads[0].Endpoints[1].Consumers; !slices.Equal(got, []string{"a/api"}) {
		t.Fatalf("an endpoint b adds carries %v, want its edges as rendered", got)
	}

	// Published caller first, then provider, the edge is delivered either way.
	callerFirst := promote(t, promote(t, base, added, "a"), added, "b")
	if got := callerFirst.Namespaces[1].Workloads[0].Endpoints[1].Consumers; !slices.Equal(got, []string{"a/api"}) {
		t.Fatalf("a's edge on b's new endpoint is lost when a is published first: %v", got)
	}
}

// TestAPromotionKeepsAnotherNamespacesEdgeOrder: the render does not sort a
// consumer list (a bootstrap Job's edges follow the dependency edges), so a
// promotion that re-sorted the lists of namespaces it leaves alone changed
// every other module's lines. A list the promotion does not change is the
// base's own, order and all; one it changes keeps the base's order and
// appends.
func TestAPromotionKeepsAnotherNamespacesEdgeOrder(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"), "c/worker", "a/api", "c/store")
	rendered := scopeCell(strings.Repeat("2", 40), digest("c"), digest("d"), "a/api", "c/store", "c/worker")

	unchanged := promote(t, base, rendered, "a")
	if got := unchanged.Namespaces[1].Workloads[0].Endpoints[0].Consumers; !slices.Equal(got, []string{"c/worker", "a/api", "c/store"}) {
		t.Fatalf("a promotion that changes none of b's edges reordered them: %v", got)
	}
	rendered.Namespaces[1].Workloads[0].Endpoints[0].Consumers = []string{"a/admin", "c/store", "c/worker"}
	changed := promote(t, base, rendered, "a")
	if got := changed.Namespaces[1].Workloads[0].Endpoints[0].Consumers; !slices.Equal(got, []string{"c/worker", "c/store", "a/admin"}) {
		t.Fatalf("a's edges were not moved in place of the base's order: %v", got)
	}
}

// TestAModuleTheRenderNoLongerDescribesStaysUntilWithdrawn: a stale checkout
// that lacks a module's render says nothing about it, so a promotion keeps
// that module's namespace and edges as the base has them; only a withdrawal —
// the composition's word or --allow-cell-removal — removes them.
func TestAModuleTheRenderNoLongerDescribesStaysUntilWithdrawn(t *testing.T) {
	base := scopeCell(strings.Repeat("1", 40), digest("a"), digest("b"), "legacy/worker")
	base.Namespaces = append(base.Namespaces, cellfile.Namespace{Name: "legacy", Module: "legacy", Workloads: []cellfile.Workload{{Name: "worker", Kind: "Deployment"}}})
	rendered := scopeCell(strings.Repeat("2", 40), digest("c"), digest("b"))

	kept := promote(t, base, rendered, "a")
	if len(kept.Namespaces) != 3 || !slices.Equal(kept.Namespaces[1].Workloads[0].Endpoints[0].Consumers, []string{"legacy/worker"}) {
		t.Fatalf("a promotion of a withdrew a module nobody withdrew: %+v", kept.Namespaces)
	}

	withdrawn, err := promotedCell(base, rendered, "a", func(module string) bool { return module == "legacy" }, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn.Namespaces) != 2 || len(withdrawn.Namespaces[1].Workloads[0].Endpoints[0].Consumers) != 0 {
		t.Fatalf("the withdrawal left legacy in the cell: %+v", withdrawn.Namespaces)
	}
	// A module the render still describes is never withdrawn by name: its
	// approval moves only with its own promotion.
	stillRendered, err := promotedCell(base, rendered, "a", func(string) bool { return true }, false)
	if err != nil {
		t.Fatal(err)
	}
	if namespaceDigest(t, stillRendered, "b") != digest("b") || len(stillRendered.Namespaces) != 2 {
		t.Fatalf("naming a rendered module moved or removed its approval: %+v", stillRendered.Namespaces)
	}
}

// TestPublishCarriesOnlyThePublishedModulesApproval runs the outage through a
// real publication: the delivery base approves payments and billing at their
// delivered builds, the checkout's render rebuilt both, and payments is
// published. The promotion commit approves payments' new build and billing's
// delivered one, and its cell diff touches nothing but payments' entries.
func TestPublishCarriesOnlyThePublishedModulesApproval(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	env := selectedEnvironment(t, workspace, "production")
	cell, data := renderedCell(t, ctx, workspace, env)
	rendered, err := cellfile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	renderedPayments := namespaceDigest(t, rendered, "payments")
	billing := func(image string) cellfile.Namespace {
		return cellfile.Namespace{Name: "billing", Module: "billing", Workloads: []cellfile.Workload{{
			Name: "ledger", Kind: "Deployment", Selector: map[string]string{"app": "ledger"}, Service: "billing/ledger",
			ServiceAccount: "ledger", SPIFFEID: "spiffe://cluster.example/ns/billing/sa/ledger", Authenticating: "ledger",
			Containers: []cellfile.Container{{Name: "ledger", Image: cellfile.Image{Repository: "ghcr.io/codefly-dev/ledger", Digest: image}}},
		}}}
	}

	// The base: payments and billing at the builds their trees run.
	base, err := cellfile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for w := range base.Namespaces[0].Workloads {
		base.Namespaces[0].Workloads[w].Containers[0].Image.Digest = digest("0")
	}
	base.Namespaces = append(base.Namespaces, billing(digest("1")))
	seedCellFile(t, remote, "environments", "production", encodeCell(t, base))

	// The checkout's render rebuilt billing too.
	rendered.Namespaces = append(rendered.Namespaces, billing(digest("2")))
	if err = os.WriteFile(cell.Path, encodeCell(t, rendered), 0o600); err != nil {
		t.Fatal(err)
	}

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Diff, digest("2")) {
		t.Fatalf("the promotion of payments approves billing's undelivered build:\n%s", plan.Diff)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	published, err := cellfile.Parse([]byte(gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+deliveryCellPath("environments", "production"))))
	if err != nil {
		t.Fatal(err)
	}
	if got := namespaceDigest(t, published, "billing"); got != digest("1") {
		t.Fatalf("the promotion of payments approves billing at %s; billing's delivered tree runs %s", got, digest("1"))
	}
	if got := namespaceDigest(t, published, "payments"); got != renderedPayments {
		t.Fatalf("the promotion of payments approves payments at %s, want its rendered build %s", got, renderedPayments)
	}
	if outside := cellChangesOutside(base, published, "payments"); len(outside) > 0 {
		t.Fatalf("the promotion of payments changes what is not its own: %v", outside)
	}
}
