// Package methodpolicy reads what each method a module package publishes
// declares to the authority, from the published contract bytes themselves
// (digest-checked against the package manifest): the one reader of the method
// table delivered to the authority, and of the routes a hop maps calls onto,
// so the two can never disagree.
package methodpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/composition"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
)

// Contract reads the method policies one published contract of a service
// serves, keyed as the authority admits a call: a protobuf contract's served
// procedures ("/pkg.Service/Method", each google.api.http route of one listed
// too, "<VERB> <template>"), or an OpenAPI contract's operations ("<VERB>
// <basePath><path>"). Unserved are the procedures its descriptor carries that
// the endpoint does not serve. Another kind of contract declares nothing.
func Contract(dir string, catalog *composition.APIContractCatalog, service string, contract *composition.ProvidedAPIContract) (methods []*compositionv1.MethodPolicyV1, unserved []string, err error) {
	switch contract.Kind {
	case composition.APIContractKindProtobuf:
		serves, servesErr := endpointServes(catalog, service, contract)
		if servesErr != nil {
			return nil, nil, servesErr
		}
		return contractMethods(filepath.Join(dir, contract.Path), contract.Digest, serves)
	case composition.APIContractKindOpenAPI:
		methods, err = routeMethods(filepath.Join(dir, contract.Path), contract.Digest)
		return methods, nil, err
	}
	return nil, nil, nil
}

// LoadCatalog reads the module's API contract catalog, or nil when the
// package has none.
func LoadCatalog(dir string) (*composition.APIContractCatalog, error) {
	catalog, err := composition.LoadAPIContractCatalog(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return catalog, err
}

// endpointServes answers which of a protobuf contract's methods its endpoint
// serves, by the package a method's service is declared in and its procedure:
// the procedures the catalog records for the endpoint, or with no catalog every
// service of the contract's package, the catalog's own default. A catalog that
// records the endpoint under another digest, or not at all, is stale.
func endpointServes(catalog *composition.APIContractCatalog, service string, contract *composition.ProvidedAPIContract) (func(pkg, procedure string) bool, error) {
	if catalog == nil {
		if contract.Package == "" {
			return nil, fmt.Errorf("endpoint %s/%s has no single protobuf package and requires %s to identify its served procedures; run codefly generate contracts", service, contract.Endpoint, composition.APIContractCatalogFileName)
		}
		return func(pkg, _ string) bool { return pkg == contract.Package }, nil
	}
	for index := range catalog.Endpoints {
		endpoint := &catalog.Endpoints[index]
		if endpoint.Service != service || endpoint.Endpoint != contract.Endpoint {
			continue
		}
		if endpoint.Digest != contract.Digest {
			return nil, fmt.Errorf("%s records endpoint %s/%s as %s, the package names %s; run codefly generate contracts", composition.APIContractCatalogFileName, service, contract.Endpoint, endpoint.Digest, contract.Digest)
		}
		procedures := map[string]bool{}
		for _, s := range endpoint.Services {
			for _, procedure := range s.Procedures {
				procedures[procedure] = true
			}
		}
		return func(_, procedure string) bool { return procedures[procedure] }, nil
	}
	return nil, fmt.Errorf("%s records no endpoint %s/%s; run codefly generate contracts", composition.APIContractCatalogFileName, service, contract.Endpoint)
}

// contractMethods reads one published contract, refusing one whose bytes are
// not the digest the package names. Only the methods serves admits are read;
// the others are returned as unserved, with no policy read and no route. A
// method bound to HTTP routes
// (google.api.http) is listed under each route too, "<VERB> <template>", with
// the same policy: the hop's HTTP face decides a REST call by its route, and
// the proto is the one place the module declares both.
func contractMethods(path, digest string, serves func(pkg, procedure string) bool) (out []*compositionv1.MethodPolicyV1, unserved []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, nil, fmt.Errorf("contract %s is %s, the package names %s; run codefly generate contracts", path, got, digest)
	}
	var set descriptorpb.FileDescriptorSet
	// Decoded against the registered types, so the policy option reads as a
	// message rather than unknown bytes.
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(data, &set); err != nil {
		return nil, nil, fmt.Errorf("contract %s does not decode: %w", path, err)
	}
	for _, file := range set.GetFile() {
		for _, service := range file.GetService() {
			for _, method := range service.GetMethod() {
				m := &compositionv1.MethodPolicyV1{Method: "/" + file.GetPackage() + "." + service.GetName() + "/" + method.GetName()}
				if file.GetPackage() == "" {
					m.Method = "/" + service.GetName() + "/" + method.GetName()
				}
				if !serves(file.GetPackage(), m.Method) {
					unserved = append(unserved, m.Method)
					continue
				}
				if options := method.GetOptions(); options != nil && proto.HasExtension(options, hopv1.E_Policy) {
					policy, _ := proto.GetExtension(options, hopv1.E_Policy).(*hopv1.Policy)
					m.AnyCheckedCaller = policy.GetAnyCheckedCaller()
					for _, r := range policy.GetRequires() {
						m.Requires = append(m.Requires, &authorityv1.WorkScopeV1{ResourceKind: r.GetResourceKind(), Actions: r.GetActions()})
					}
				}
				out = append(out, m)
				for _, route := range httpRoutes(method.GetOptions()) {
					bound, ok := proto.Clone(m).(*compositionv1.MethodPolicyV1)
					if !ok {
						return nil, nil, fmt.Errorf("cannot clone policy for %s", m.GetMethod())
					}
					bound.Method = route
					out = append(out, bound)
				}
			}
		}
	}
	return out, unserved, nil
}

// httpRoutes are the routes a method's google.api.http option binds it to,
// "<VERB> <template>", the first binding and each additional one. A custom
// verb's binding is listed under the verb it names.
func httpRoutes(options *descriptorpb.MethodOptions) []string {
	if options == nil || !proto.HasExtension(options, annotations.E_Http) {
		return nil
	}
	rule, _ := proto.GetExtension(options, annotations.E_Http).(*annotations.HttpRule)
	var out []string
	for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
		var verb, template string
		switch pattern := r.GetPattern().(type) {
		case *annotations.HttpRule_Get:
			verb, template = "GET", pattern.Get
		case *annotations.HttpRule_Put:
			verb, template = "PUT", pattern.Put
		case *annotations.HttpRule_Post:
			verb, template = "POST", pattern.Post
		case *annotations.HttpRule_Delete:
			verb, template = "DELETE", pattern.Delete
		case *annotations.HttpRule_Patch:
			verb, template = "PATCH", pattern.Patch
		case *annotations.HttpRule_Custom:
			verb, template = strings.ToUpper(pattern.Custom.GetKind()), pattern.Custom.GetPath()
		}
		if verb != "" && template != "" {
			out = append(out, verb+" "+plainTemplate(template))
		}
	}
	return out
}

// plainTemplate spells a google.api.http template with one "{name}" per
// segment a variable takes: "{name=*}" is "{name}", "{name=a/*}" is "a/{name}",
// "{name=a/**}" is "a/{name=**}", and "{name=**}" stays, the one form that
// takes the rest of the path. The authority matches segments, not variables.
func plainTemplate(template string) string {
	var out strings.Builder
	for {
		open := strings.IndexByte(template, '{')
		if open < 0 {
			out.WriteString(template)
			return out.String()
		}
		end := strings.IndexByte(template[open:], '}')
		if end < 0 {
			out.WriteString(template)
			return out.String()
		}
		out.WriteString(template[:open])
		name, sub, bound := strings.Cut(template[open+1:open+end], "=")
		switch {
		case !bound || sub == "*":
			out.WriteString("{" + name + "}")
		case sub == "**":
			out.WriteString("{" + name + "=**}")
		default:
			segments := strings.Split(sub, "/")
			for i, segment := range segments {
				switch segment {
				case "*":
					segments[i] = "{" + name + "}"
				case "**":
					segments[i] = "{" + name + "=**}"
				}
			}
			out.WriteString(strings.Join(segments, "/"))
		}
		template = template[open+end+1:]
	}
}

// PolicyExtension is the OpenAPI operation extension that declares what a call
// to the route must hold: the authority.hop.v1.Policy a proto method marks
// itself with, as its JSON.
const PolicyExtension = "x-codefly-authority-policy"

// routeMethods reads one published OpenAPI contract: each operation is the HTTP
// route "<VERB> <path>", and what it declares is its PolicyExtension. An
// operation that declares nothing is listed with neither field set, as an
// unmarked proto method is, so the authority refuses it by name.
func routeMethods(path, digest string) ([]*compositionv1.MethodPolicyV1, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("contract %s is %s, the package names %s; run codefly generate contracts", path, got, digest)
	}
	var document struct {
		BasePath string                                `json:"basePath"`
		Paths    map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("contract %s does not decode: %w", path, err)
	}
	prefix := strings.TrimSuffix(document.BasePath, "/")
	var out []*compositionv1.MethodPolicyV1
	for route, operations := range document.Paths {
		for verb, raw := range operations {
			if !httpVerbs[verb] {
				continue // parameters, summary and the like sit beside the operations
			}
			m := &compositionv1.MethodPolicyV1{Method: strings.ToUpper(verb) + " " + prefix + route}
			var operation map[string]json.RawMessage
			if err := json.Unmarshal(raw, &operation); err != nil {
				return nil, fmt.Errorf("contract %s: %s: %w", path, m.Method, err)
			}
			if declared, ok := operation[PolicyExtension]; ok {
				var policy hopv1.Policy
				if err := protojson.Unmarshal(declared, &policy); err != nil {
					return nil, fmt.Errorf("contract %s: %s declares %s that is not an authority.hop.v1.Policy: %w", path, m.Method, PolicyExtension, err)
				}
				m.AnyCheckedCaller = policy.GetAnyCheckedCaller()
				for _, r := range policy.GetRequires() {
					m.Requires = append(m.Requires, &authorityv1.WorkScopeV1{ResourceKind: r.GetResourceKind(), Actions: r.GetActions()})
				}
			}
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetMethod() < out[j].GetMethod() })
	return out, nil
}

var httpVerbs = map[string]bool{"get": true, "put": true, "post": true, "delete": true, "patch": true, "head": true, "options": true}
