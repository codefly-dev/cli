package gitops

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
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
)

// MethodPolicies reads what each method a module's published protobuf contracts
// serve declares with the authority's policy option, from the module package
// at dir. Every served method is listed, a method with no mark with neither
// field set, so the authority tells "declares nothing" from "not served".
// A module with no package declares no method policy: ok is false.
func MethodPolicies(dir string) (methods []*compositionv1.MethodPolicyV1, ok bool, err error) {
	foreign, err := foreignMethods(dir)
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Stat(filepath.Join(dir, composition.PackageManifestFileName)); os.IsNotExist(err) {
		return foreign, len(foreign) > 0, nil
	}
	manifest, err := composition.LoadPackageManifest(dir)
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	for _, service := range manifest.Services {
		for _, contract := range service.APIContracts {
			read := contractMethods
			switch contract.Kind {
			case "protobuf":
			case composition.APIContractKindOpenAPI:
				read = routeMethods
			default:
				continue
			}
			served, err := read(filepath.Join(dir, contract.Path), contract.Digest)
			if err != nil {
				return nil, false, fmt.Errorf("module package %s, service %s: %w", manifest.ID, service.Name, err)
			}
			for _, m := range served {
				if !seen[m.GetMethod()] {
					seen[m.GetMethod()] = true
					methods = append(methods, m)
				}
			}
		}
	}
	// A foreign method is one no published contract serves: a module that
	// declares one it also publishes would have two answers for one method.
	for _, m := range foreign {
		if seen[m.GetMethod()] {
			return nil, false, fmt.Errorf("module package %s declares foreign method %s, which its published contracts already serve", manifest.ID, m.GetMethod())
		}
		seen[m.GetMethod()] = true
		methods = append(methods, m)
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].GetMethod() < methods[j].GetMethod() })
	return methods, true, nil
}

// foreignMethods reads the methods the module at dir serves from a protocol
// it does not own, from its module contract, each with what a call requires.
// A module that publishes no contract declares none.
func foreignMethods(dir string) ([]*compositionv1.MethodPolicyV1, error) {
	contract, err := modulecontract.Load(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*compositionv1.MethodPolicyV1
	for _, method := range contract.ForeignMethods {
		m := &compositionv1.MethodPolicyV1{Method: method.Method}
		for _, required := range method.Requires {
			m.Requires = append(m.Requires, &authorityv1.WorkScopeV1{ResourceKind: required.ResourceKind, Actions: required.Actions})
		}
		out = append(out, m)
	}
	return out, nil
}

// contractMethods reads one published contract, refusing one whose bytes are
// not the digest the package names. A method bound to HTTP routes
// (google.api.http) is listed under each route too, "<VERB> <template>", with
// the same policy: the hop's HTTP face decides a REST call by its route, and
// the proto is the one place the module declares both.
func contractMethods(path, digest string) ([]*compositionv1.MethodPolicyV1, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("contract %s is %s, the package names %s; run codefly generate contracts", path, got, digest)
	}
	var set descriptorpb.FileDescriptorSet
	// Decoded against the registered types, so the policy option reads as a
	// message rather than unknown bytes.
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("contract %s does not decode: %w", path, err)
	}
	var out []*compositionv1.MethodPolicyV1
	for _, file := range set.GetFile() {
		for _, service := range file.GetService() {
			for _, method := range service.GetMethod() {
				m := &compositionv1.MethodPolicyV1{Method: "/" + file.GetPackage() + "." + service.GetName() + "/" + method.GetName()}
				if file.GetPackage() == "" {
					m.Method = "/" + service.GetName() + "/" + method.GetName()
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
					bound := proto.Clone(m).(*compositionv1.MethodPolicyV1)
					bound.Method = route
					out = append(out, bound)
				}
			}
		}
	}
	return out, nil
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

// ScopeCeilings reads the module's own resource kinds with the actions its
// module contract declares on each, from the module at dir. A module that
// publishes no contract declares none.
func ScopeCeilings(dir string) ([]*authorityv1.WorkScopeV1, error) {
	contract, err := modulecontract.Load(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*authorityv1.WorkScopeV1
	for _, ceiling := range contract.ScopeCeilings {
		out = append(out, &authorityv1.WorkScopeV1{ResourceKind: ceiling.ResourceKind, Actions: append([]string(nil), ceiling.Actions...)})
	}
	return out, nil
}
