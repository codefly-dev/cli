package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/authority/libraries/authority-core/go/decide"
	"github.com/codefly-dev/authority/libraries/authority-core/go/domain"
	"github.com/codefly-dev/cli/pkg/gitops"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/stretchr/testify/require"
)

func storeTypes(actions ...string) decide.Scopes {
	return decide.Scopes{{ResourceKind: "store.types", Actions: actions}}
}

// A composition with the shape that cost a day: a module asks to read and
// define alone, and the envelope grants define with no read.
func deliveredFixture() *gitops.DeliveredAuthority {
	shelf := domain.Module{Name: "shelf", Principal: "shelf", Generation: 1, OwnerAware: true, ResourceKinds: storeTypes("define", "read"),
		Methods: map[string]domain.MethodPolicy{"/store.v1.Store/GetType": {Requires: storeTypes("read")}}}
	catalog := domain.Module{Name: "catalog", Principal: "catalog", Generation: 1, Bindings: map[string]domain.Binding{
		"types": {ID: "types", Audience: "shelf", HeadlessScopes: storeTypes("define", "read")},
		"rows":  {ID: "rows", Audience: "shelf", InvokeScopes: storeTypes("read")}}}
	return &gitops.DeliveredAuthority{
		Modules:      map[string]gitops.DeliveredModule{"shelf": {Module: shelf, File: "shelf-paired.yaml"}, "catalog": {Module: catalog, File: "catalog-paired.yaml"}},
		EnvelopeFile: "module-envelope.yaml",
		Envelope:     map[string]map[string]domain.Binding{"catalog": {"types": {Audience: "shelf", HeadlessScopes: storeTypes("define")}}},
	}
}

func TestTheGateNamesTheFileAndFieldOfEveryDisagreement(t *testing.T) {
	d := deliveredFixture()
	findings := strings.Join(deliveredFindings(d), "\n")
	require.Contains(t, findings, "module-envelope.yaml: principals[catalog].operation_audiences[types].headless_scopes withholds store.types:read, which catalog-paired.yaml asks for in authority.bindings[types]")
	require.Contains(t, findings, "module-envelope.yaml: principals[catalog].operation_audiences[rows] is absent")
	require.Len(t, deliveredFindings(d), 2)

	// A delivery or an envelope that could not be read is a finding, never a pass.
	d.Modules["broken"] = gitops.DeliveredModule{File: "broken-paired.yaml", Unread: "decode: bad"}
	d.EnvelopeUnread = "unknown field"
	findings = strings.Join(deliveredFindings(d), "\n")
	require.Contains(t, findings, "broken-paired.yaml: could not be read as a delivery: decode: bad")
	require.Contains(t, findings, "the module envelope could not be read, so no binding is known to be granted")

	d = deliveredFixture()
	d.Envelope["catalog"] = map[string]domain.Binding{"types": d.Modules["catalog"].Module.Bindings["types"], "rows": d.Modules["catalog"].Module.Bindings["rows"]}
	require.Empty(t, deliveredFindings(d), "an envelope granting what is asked agrees")
}

func TestATracePrintsEveryLinkAndNeverPrintsAnUnreadOneAsAPass(t *testing.T) {
	d := deliveredFixture()
	allowed := d.Envelope["catalog"]["types"]
	call := decide.Call{Caller: d.Modules["catalog"].Module, Audience: d.Modules["shelf"].Module, Binding: "types", Method: "/store.v1.Store/GetType",
		Installation: domain.Installation{PrincipalID: "p"}, Envelope: &allowed}
	files := map[string]string{decide.LinkEnvelope: d.EnvelopeFile}
	shelf := d.Modules["shelf"]
	var out bytes.Buffer
	err := printTrace(&out, &call, decide.Trace(call), files, &shelf)
	require.ErrorContains(t, err, "link 6 (envelope) is refused")
	text := out.String()
	for _, want := range []string{
		"1 page registered        UNREAD", "3 role held              UNREAD", "this link is not checked, and is not a pass",
		"5 ceiling                ADMITTED", "6 envelope               REFUSED", "file     module-envelope.yaml",
		"field    principals[catalog].operation_audiences[types].headless_scopes", "value    store.types:define",
		"headless store.types:read", "7 policy                 REFUSED", "8 owner                  REFUSED",
		"FIRST LINK THAT DOES NOT ADMIT THE CALL: 6 envelope (refused) module-envelope.yaml",
	} {
		require.Contains(t, text, want)
	}

	// The audience's delivery unread: its links have no verdict, whatever was computed.
	out.Reset()
	require.Error(t, printTrace(&out, &call, decide.Trace(call), files, &gitops.DeliveredModule{}))
	require.Contains(t, out.String(), "7 policy                 UNREAD")
	require.Contains(t, out.String(), "8 owner                  UNREAD")
}

// A generated table saying something else than the published contract fails
// by method; with no contract to read, that is the finding.
func TestAGeneratedTableIsHeldToThePublishedPolicies(t *testing.T) {
	dir := t.TempDir()
	findings := tableFindings(dir)
	require.Len(t, findings, 2)
	require.Contains(t, findings[0], "publishes no method policies")
	require.Contains(t, findings[1], "no generated permissions_gen.go")

	table := filepath.Join(dir, "permissions_gen.go")
	require.NoError(t, os.WriteFile(table, []byte("\t\"/a/Get\": {AnyCheckedCaller: true, Requires: []Permission{}},\n"), 0o644))
	read := domain.MethodPolicy{Requires: []*basev0.WorkScopeV1{{ResourceKind: "a.things", Actions: []string{"read"}}}}
	stale, err := gitops.PermissionTable(table)
	require.NoError(t, err)
	links := decide.Copies(read, true, []decide.Statement{{Where: table, Declared: true, Policy: stale["/a/Get"]}})
	require.Equal(t, decide.Refused, links[0].Verdict)
	require.Contains(t, links[0].Reason, `it says "any checked caller" and the policy it must equal says "requires a.things:read"`)
}

// Open on a module that checks owners is listed, every method, Version too:
// an honest unproven, never a filtered ok. A module checking no owner has none.
func TestTheGateListsEveryMethodNoDeclarationDecides(t *testing.T) {
	shelf := deliveredFixture().Modules["shelf"].Module
	shelf.Methods["/store.v1.Store/Version"] = domain.MethodPolicy{AnyCheckedCaller: true}
	listed := unstated("shelf-paired.yaml", &shelf)
	require.Len(t, listed, 1, "a method that requires a scope is decided by its declaration")
	require.Contains(t, listed[0], "shelf-paired.yaml: methods[/store.v1.Store/Version]: shelf checks owners and declares")
	shelf.OwnerAware = false
	require.Empty(t, unstated("shelf-paired.yaml", &shelf))
}

// A checkout whose table says something else than what the environment
// delivers is a refused copy: the branch moved and the delivery did not.
func TestACheckoutIsHeldToWhatTheEnvironmentDelivers(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "permissions_gen.go"),
		[]byte("\t\"/store.v1.Store/GetType\": {AnyCheckedCaller: true, Requires: []Permission{}},\n"), 0o644))
	findings := strings.Join(undeliveredFindings(deliveredFixture(), "shelf", dir), "\n")
	require.Contains(t, findings, `permissions_gen.go: /store.v1.Store/GetType: it says "any checked caller" and the policy it must equal says "requires store.types:read"`)
	require.Contains(t, findings, "(delivered by shelf-paired.yaml)")
	require.Contains(t, findings, "publishes no method policies", "the unread contract is its own finding")

	absent := undeliveredFindings(deliveredFixture(), "elsewhere", dir)
	require.Len(t, absent, 1)
	require.Contains(t, absent[0], `delivers no readable module "elsewhere"`)
}
