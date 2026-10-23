package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/core/composition"
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
	if _, err := os.Stat(filepath.Join(dir, composition.PackageManifestFileName)); os.IsNotExist(err) {
		return nil, false, nil
	}
	manifest, err := composition.LoadPackageManifest(dir)
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	for _, service := range manifest.Services {
		for _, contract := range service.APIContracts {
			if contract.Kind != "protobuf" {
				continue
			}
			served, err := contractMethods(filepath.Join(dir, contract.Path), contract.Digest)
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
	sort.Slice(methods, func(i, j int) bool { return methods[i].GetMethod() < methods[j].GetMethod() })
	return methods, true, nil
}

// contractMethods reads one published contract, refusing one whose bytes are
// not the digest the package names.
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
			}
		}
	}
	return out, nil
}
