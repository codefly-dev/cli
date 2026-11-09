package add

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"gopkg.in/yaml.v3"
)

// A module page is a module that ships no solution manifest and declares a
// page beside its entry service. Its page calls the module's own procedures
// on the host's module edge — `<moduleApiBase>/<package.Service>/<Method>`,
// the `/_browser` door — which resolves the procedure against the module's
// published routes, checks the person's Work Context and forwards the call
// through the authority's hop to the entry's Connect endpoint. So the entry
// serves Connect, every method it publishes carries the policy the hop checks
// (authority.hop.v1.policy), and the roles a person is granted are spelled in
// exactly those policies.
//
// pageOperations is the ONE statement of what the scaffold publishes. The
// proto, the scope ceilings, every role and the comment beside each of a
// role's scopes are derived from it, so a role can grant only what an
// operation requires and a required scope cannot be missing from its role.

// pageOperation is one method the entry serves and what a call to it must hold.
type pageOperation struct {
	method   string
	doc      string // what the method does, in the proto and beside the scopes
	request  string // the request message's fields
	response string // the response message's fields
	// kind is the resource kind after the module's name ("entry" for
	// <module>.entry), and action the action on it. Empty: any checked caller.
	kind, action string
	// roles are the roles whose holders call it.
	roles []string
}

const (
	editorRole = "editor"
	viewerRole = "viewer"
)

// pageRoles is the order roles are written in: installer (required by name),
// then the write position, then the read one.
var pageRoles = []string{modulecontract.InstallerRole, editorRole, viewerRole}

var pageOperations = []pageOperation{
	{
		method:   "Version",
		doc:      "reports the running version. Any checked caller may ask: it answers nothing about the organization",
		response: "string version = 1;",
	},
	{
		method: "ListEntries", kind: "entry", action: "read",
		doc:      "answers the organization's entries; the page calls it when it opens",
		response: "repeated string entries = 1;",
		// Whoever administers the page opens it too.
		roles: []string{modulecontract.InstallerRole, editorRole, viewerRole},
	},
	{
		method: "PutEntry", kind: "entry", action: "write",
		doc:     "records one entry",
		request: "string text = 1;",
		roles:   []string{editorRole},
	},
	{
		method: "ConfigurePage", kind: "page", action: "configure",
		doc:     "changes how the page presents itself to the organization",
		request: "string title = 1;",
		roles:   []string{modulecontract.InstallerRole},
	},
}

// pageKind is a resource kind of the module's own vocabulary.
func pageKind(module, kind string) string { return module + "." + kind }

// pageScopes is what the operations a role's holders call require, kind by
// kind in the order the operations name them. With role "" it is every scope
// any operation requires: the module's ceilings.
func pageScopes(module, role string) []modulecontract.CeilingScope {
	var scopes []modulecontract.CeilingScope
	for _, operation := range pageOperations {
		if operation.kind == "" || (role != "" && !slices.Contains(operation.roles, role)) {
			continue
		}
		kind := pageKind(module, operation.kind)
		index := slices.IndexFunc(scopes, func(scope modulecontract.CeilingScope) bool { return scope.ResourceKind == kind })
		if index < 0 {
			scopes = append(scopes, modulecontract.CeilingScope{ResourceKind: kind})
			index = len(scopes) - 1
		}
		if !slices.Contains(scopes[index].Actions, operation.action) {
			scopes[index].Actions = append(scopes[index].Actions, operation.action)
		}
	}
	return scopes
}

// modulePageContract is the module contract of a module page: roles derived
// from the operations, no standing authority for the installation, and every
// published method held to an explicit policy.
func modulePageContract(module, entry string) *modulecontract.Contract {
	contract := &modulecontract.Contract{
		Schema:     modulecontract.SchemaV1,
		Principal:  module,
		Namespaces: []string{},
		Queues:     []string{},
		// A module page holds no standing authority: it is installed by
		// composition in every organization, and acts only for the person
		// whose call it serves.
		InstallationRole: "none",
		Bindings:         []modulecontract.Binding{},
		Destinations: []modulecontract.Destination{{
			ID: module + "-" + standards.CONNECT, Service: entry, Endpoint: standards.CONNECT, Kind: modulecontract.DestinationModule,
		}},
		// A method the entry publishes with no policy is refused before
		// anything renders, rather than per call at the hop.
		RequireMethodPolicies: true,
	}
	for _, scope := range pageScopes(module, "") {
		contract.ScopeCeilings = append(contract.ScopeCeilings, modulecontract.ScopeCeiling{ResourceKind: scope.ResourceKind, Actions: scope.Actions})
	}
	for _, role := range pageRoles {
		contract.Roles = append(contract.Roles, modulecontract.Role{Name: role, Scopes: pageScopes(module, role)})
	}
	return contract
}

// modulePageContractPreamble says what the contract is, in the file.
const modulePageContractPreamble = `# What this module page asks for (codefly/module-contract/v1). A request, never
# a grant.
#
# ACCESS. The module is installed by composition in every organization that
# composes it; there is no install step and nobody can use the page until an
# administrator grants one of its roles ("Set up access" in Admin, which assigns
# the teams <module>-installer, <module>-editor and <module>-viewer). A person
# reaches the page only through a role a team of theirs holds.
#
# LEAST PRIVILEGE. Each role grants exactly what the operations its holders
# call require, and each scope below names the operation that needs it. Every
# operation's requirement is its authority.hop.v1.policy in the entry's proto:
# change a policy there, then the scope ceilings and the roles here, together.
# "codefly doctor solution --dir <module> --roles" prints what each role
# reaches, and warns on a grant no operation requires and an operation no role
# reaches.
`

// marshalModulePageContract renders the contract with, above each scope a
// role grants, the operations that need it.
func marshalModulePageContract(module string, contract *modulecontract.Contract) ([]byte, error) {
	var document yaml.Node
	if err := document.Encode(contract); err != nil {
		return nil, err
	}
	why := map[string][]string{} // "<kind>:<action>" -> what needs it
	for _, operation := range pageOperations {
		if operation.kind != "" {
			key := pageKind(module, operation.kind) + ":" + operation.action
			why[key] = append(why[key], fmt.Sprintf("%s (%s)", operation.method, operation.doc))
		}
	}
	if roles := yamledit.MapValue(&document, "roles"); roles != nil {
		for _, role := range roles.Content {
			scopes := yamledit.MapValue(role, "scopes")
			if scopes == nil {
				continue
			}
			for _, scope := range scopes.Content {
				kind := yamledit.MapValue(scope, "resource_kind").Value
				var lines []string
				for _, action := range yamledit.MapValue(scope, "actions").Content {
					for _, reason := range why[kind+":"+action.Value] {
						lines = append(lines, fmt.Sprintf("%s:%s: %s", kind, action.Value, reason))
					}
				}
				scope.HeadComment = strings.Join(lines, "\n")
			}
		}
	}
	rendered, err := yaml.Marshal(&document)
	if err != nil {
		return nil, err
	}
	return []byte(modulePageContractPreamble + string(rendered)), nil
}

//go:embed vendored/authority/hop/v1/policy.proto
var hopPolicyProto []byte

// hopPolicyProtoPath is where the vendored option proto sits under proto/,
// at the import path api.proto names it by.
const hopPolicyProtoPath = "authority/hop/v1/policy.proto"

// hopPolicyGoPackage is the Go package that registers the option's
// descriptor: the authority's hop library. buf.gen.yaml maps the import onto
// it and generates only api.proto, so the descriptor is registered once.
const hopPolicyGoPackage = "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"

// hopLibraryModule is the Go module holding hopPolicyGoPackage, which the
// entry's go.mod must require once api.proto imports the option.
const hopLibraryModule = "github.com/codefly-dev/authority/libraries/hop/go"

// modulePageProto is the entry's API: one service, every method marked with
// what a call to it must hold.
func modulePageProto(module, goModule, protoService string) string {
	var out strings.Builder
	fmt.Fprintf(&out, `syntax = "proto3";
package api;

// Keep the Go import path stable while the protobuf package remains free to
// evolve into a versioned wire namespace.
option go_package = "%s/pkg/gen;gen";

// Each method declares what a call to it must hold (authority.hop.v1.policy).
// The hop in front of this service applies it to every call, and the module's
// roles (module.contract.codefly.yaml) are spelled in exactly these terms.
import "%s";
`, goModule, hopPolicyProtoPath)
	for _, operation := range pageOperations {
		fmt.Fprintf(&out, "\n// %sRequest is what %s is asked.\nmessage %sRequest {%s}\n", operation.method, operation.method, operation.method, protoFields(operation.request))
		fmt.Fprintf(&out, "\n// %sResponse is what %s answers.\nmessage %sResponse {%s}\n", operation.method, operation.method, operation.method, protoFields(operation.response))
	}
	fmt.Fprintf(&out, "\n// %s is the %s module's API, which its page calls on the host's module edge.\nservice %s {\n", protoService, module, protoService)
	for index, operation := range pageOperations {
		if index > 0 {
			out.WriteString("\n")
		}
		policy := "{any_checked_caller: true}"
		if operation.kind != "" {
			policy = fmt.Sprintf("{requires: [{resource_kind: %q, actions: [%q]}]}", pageKind(module, operation.kind), operation.action)
		}
		fmt.Fprintf(&out, "    // %s %s.\n    rpc %s(%sRequest) returns (%sResponse) {\n        option (authority.hop.v1.policy) = %s;\n    }\n",
			operation.method, operation.doc, operation.method, operation.method, operation.method, policy)
	}
	out.WriteString("}\n")
	return out.String()
}

func protoFields(fields string) string {
	if fields == "" {
		return ""
	}
	return "\n    " + fields + "\n"
}

// modulePageBufYAML lints the entry's own proto and leaves the vendored option
// proto alone: it is not authored here.
const modulePageBufYAML = `version: v1
lint:
  use:
    - DEFAULT
    - COMMENTS
  ignore:
    - authority
`

// modulePageBufGen generates Go, gRPC and Connect from api.proto alone, with
// the option import mapped onto the hop library that registers it. One
// listener family: no gateway and no OpenAPI, the module serves no REST.
func modulePageBufGen() string {
	var out strings.Builder
	out.WriteString(`version: v2
# Go package identity is explicit in api.proto. authority/ is import-only: the
# authority's hop library registers that descriptor, so only api.proto is
# generated and every plugin maps the import onto the library's package.
managed:
  enabled: false
inputs:
  - directory: .
    paths:
      - api.proto
plugins:
`)
	for _, plugin := range []string{"protoc-gen-go", "protoc-gen-go-grpc", "protoc-gen-connect-go"} {
		fmt.Fprintf(&out, "  - local: %s\n    out: ../code/pkg/gen\n    opt:\n      - paths=source_relative\n      - M%s=%s\n", plugin, hopPolicyProtoPath, hopPolicyGoPackage)
	}
	return out.String()
}

// modulePageConnectEndpoint is the entry's one endpoint: Connect, which also
// serves gRPC and gRPC-Web on the same port. Internal: the hop is the one
// thing that reaches it.
func modulePageConnectEndpoint() resources.Endpoint {
	return resources.Endpoint{Name: standards.CONNECT, API: standards.CONNECT, Visibility: resources.VisibilityInternal}
}

// modulePageInterface exports the entry's Connect endpoint, the one the host's
// module edge forwards a page's call to. An endpoint the interface does not
// export admits no host gateway.
func modulePageInterface(entry string) *resources.ModuleInterface {
	return &resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{
		{Service: entry, Endpoint: standards.CONNECT, Visibility: resources.VisibilityInternal},
	}}
}

// modulePagePackage is the module's package identity: the solution scaffold's
// derivation, publishing the entry's Connect endpoint.
func modulePagePackage(id, toolVersion, declaredFloor, entry string) (*composition.PackageManifest, error) {
	packaged, err := solutionPackage(id, toolVersion, declaredFloor)
	if err != nil {
		return nil, err
	}
	packaged.Services = []composition.ProvidedService{{Name: entry, Endpoints: []string{standards.CONNECT}}}
	return packaged, nil
}

// modulePageReadme is the module's README: what the page is, and how access
// to it works.
func modulePageReadme(module, title, entry, client string) string {
	return fmt.Sprintf(`# %[2]s

The %[1]s module and its page. The page (services/fe-remote, exposing
./Page) is mounted by a host; it calls this module's own procedures on the
host's module edge, %[5]s<moduleApiBase>/api.%[4]s/<Method>%[5]s, through the
generated client in libraries/%[6]s. The edge checks the person's Work Context
against each method's policy (services/%[3]s/proto/api.proto) and forwards the
call through the authority's hop to services/%[3]s.

## How access works

- **Installed by composition.** Every organization whose platform composes
  this module has it; nobody reviews or installs it.
- **Nobody can use it until a role is granted.** Holding a role is the only
  way to reach the page, and nobody holds one by being an administrator.
- **Set up access.** An administrator opens Admin, finds the page under "Set
  up access", ticks the roles they want for themselves, and confirms: the host
  creates or reuses the teams %[5]s%[1]s-installer%[5]s, %[5]s%[1]s-editor%[5]s and
  %[5]s%[1]s-viewer%[5]s, grants each its role, and adds the administrator to the
  ticked ones. Anyone added to a team later holds its role.

The roles grant only what the operations their holders call require
(module.contract.codefly.yaml names the operation beside each scope):

| Role | Can |
| --- | --- |
%[7]s
%[5]scodefly doctor solution --dir modules/%[1]s --roles%[5]s prints what each role reaches.

## Build and check

    codefly generate contracts %[1]s     # after editing services/%[3]s/proto/api.proto
    codefly generate permissions %[1]s
    codefly generate client --from %[1]s/%[3]s/connect --language typescript --name %[6]s --npm-scope %[6]s --force
    cd services/fe-remote && npm run client && npm install && npm run build
    codefly doctor solution --dir modules/%[1]s --roles

The scaffolded methods answer Unimplemented until you implement them in
services/%[3]s/code (Configuration.Service, set from a Configure hook).
`, module, title, entry, pageProtoService(module), "`", client, pageRoleTable(module))
}

// pageRoleTable is one row per role: the operations its holders call.
func pageRoleTable(module string) string {
	rows := make([]string, 0, len(pageRoles))
	for _, role := range pageRoles {
		var calls []string
		for _, operation := range pageOperations {
			if slices.Contains(operation.roles, role) {
				calls = append(calls, fmt.Sprintf("%s (%s:%s)", operation.method, pageKind(module, operation.kind), operation.action))
			}
		}
		rows = append(rows, fmt.Sprintf("| %s | %s |", role, strings.Join(calls, ", ")))
	}
	return strings.Join(rows, "\n") + "\n"
}

// pageProtoService is the proto service the go-grpc agent names after the
// entry service, which the scaffold names after the module.
func pageProtoService(module string) string {
	return shared.ToTitle(module) + "Service"
}
