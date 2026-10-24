package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
)

// TestTheModuleContractKitRunsThroughTheRender drives core's module-contract
// kit through this repository's own entrypoint — the authority derivation,
// which reads the contract a module publishes — rather than through core's
// parser, which would prove nothing about this reader. Every fixture core
// refuses is refused here with the same sentinel and reason. Every fixture
// core accepts is read, and then either derives the manifest's authority or is
// refused by the derivation for the ONE reason it names past the reader (a
// destination naming an endpoint the module does not declare); which accepted
// fixtures that is holds as a fixed set, so a derivation failing for any
// other reason — an unresolved slot, a configuration it cannot read, a
// defect — fails the kit instead of passing as acceptance.
func TestTheModuleContractKitRunsThroughTheRender(t *testing.T) {
	ctx := context.Background()
	workspace, _ := writeAuthorityWorkspace(t)
	// The kit's accepted fixtures are a module named "assistant" — the
	// principal every credential of a module is issued to is the module's own
	// name — whose slots resolve from the assistant group; the composition
	// is given that module so the derivation runs past the reader.
	configuration := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	config, err := os.ReadFile(configuration)
	require.NoError(t, err)
	require.Contains(t, string(config), "modules:\n  - name: shop\n")
	require.NoError(t, os.WriteFile(configuration, []byte(strings.Replace(string(config), "modules:\n  - name: shop\n", "modules:\n  - name: shop\n  - name: assistant\n  - name: documents\n  - name: model-gateway\n", 1)), 0o644))
	// The callees a binding that names its calls is resolved against
	// (binding-names-calls): each declares what its calls require in its own
	// published contract, a protobuf one and an OpenAPI one.
	writeCalleePackages(t, workspace.Dir())
	for rel, content := range map[string]string{
		filepath.Join("modules", "assistant", resources.ModuleConfigurationName):                     "kind: module\nname: assistant\nservices:\n  - name: api\n",
		filepath.Join("modules", "assistant", "services", "api", resources.ServiceConfigurationName): hostedServiceYAML("api", "assistant") + "module-identity: true\n",
		filepath.Join("modules", "assistant", "module.package.codefly.yaml"):                         "schema: codefly/module-package/v1\nid: acme/assistant\nversion: 1.0.0\n",
		filepath.Join("configurations", "staging", "assistant.env"):                                  "MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nMODEL_BINDING=model\nEVIDENCE_AUDIENCE=documents\nEVIDENCE_RESOURCE_KIND=documents.passages\nEVIDENCE_BINDING=evidence\nANNOTATIONS_PREFIX=annotations\n",
	} {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "assistant")
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "assistant", "api")
	fixtures, err := modulecontract.Fixtures()
	require.NoError(t, err)
	names := map[string]string{}
	for _, fixture := range fixtures {
		names[string(fixture.Document)] = fixture.Name
	}
	var uncarried []string
	modulecontract.Run(t, func(document []byte) error {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), document, 0o600))
		authority, err := shopAuthority(t, workspace, module, services, env)
		if names[string(document)] == "binding-names-calls" && err == nil {
			// The ceilings are what the callees declare for the calls named,
			// and nothing the module wrote.
			want := map[string]string{
				"evidence": "invoke=documents.collection:read,documents:read lookup=documents:read",
				"model":    "invoke=modelservice.profiles:invoke,modelservice.profiles:read",
			}
			for _, binding := range authority.Bindings {
				var got []string
				for _, operation := range []string{"invoke", "lookup", "headless"} {
					if scopes, ok := binding.Operations[operation]; ok {
						got = append(got, operation+"="+strings.Join(scopes, ","))
					}
				}
				require.Equal(t, want[binding.ID], strings.Join(got, " "), "binding %s", binding.ID)
			}
		}
		verdict, past := kitVerdict(authority, err)
		if past {
			uncarried = append(uncarried, names[string(document)])
		}
		return verdict
	})
	sort.Strings(uncarried)
	// The accepted fixtures declaring a destination naming an endpoint this
	// kit's module does not declare, and no other.
	require.Equal(t, []string{"upper-case-slot-key", "valid"}, uncarried)
}

// kitVerdict maps the renderer's verdict on a kit document to what the kit
// checks. Core's sentinels pass through: the reader refused. A contract the
// reader accepted and the derivation refused for the one reason it names past
// the reader (errUnrenderedDestination) is acceptance for the kit, reported as
// past so the test can hold the set of such fixtures. A success must have
// derived an authority. Anything else is a failure the kit must see: mapping it
// to acceptance would make the run green about a name.
func kitVerdict(authority *solutionhost.Authority, err error) (verdict error, past bool) {
	switch {
	case err == nil:
		if authority == nil {
			return errors.New("the contract was read and resolved but derived no authority"), false
		}
		return nil, false
	case errors.Is(err, modulecontract.ErrInvalid), errors.Is(err, modulecontract.ErrSchema):
		return err, false
	case errors.Is(err, errUnrenderedDestination):
		return nil, true
	}
	return fmt.Errorf("the renderer failed for a reason the kit does not name, so this run proves nothing about the reader: %w", err), false
}

// TestTheResolutionKitRunsThroughTheRendersProvider: core's resolution kit
// drives the provider the render resolves slots from, built from one group's
// records the way the workspace configurations hold them — competing
// spellings, a key supplied twice, a public and a secret occurrence, a
// missing key, a multi-line or empty value, and the resolved resource kinds.
// A provider that selected among the records would fail it.
func TestTheResolutionKitRunsThroughTheRendersProvider(t *testing.T) {
	modulecontract.RunResolution(t, workspaceValuesOf)
}

// workspaceValuesOf is the render's provider over one group's records, held
// the way a workspace configuration read holds them: one information named
// for the group, carrying every record as a configuration value.
func workspaceValuesOf(group string, records []modulecontract.Record) modulecontract.Values {
	values := make([]*basev0.ConfigurationValue, 0, len(records))
	for _, record := range records {
		values = append(values, &basev0.ConfigurationValue{Key: record.Key, Value: record.Value, Secret: record.Secret})
	}
	return workspaceValues{provided: &configurations.WorkspaceConfigurations{
		Infos: []*basev0.ConfigurationInformation{{Name: group, ConfigurationValues: values}},
	}}
}

// TestTheModuleContractKitAdapterFailsOnUnrelatedErrors: the adapter between
// the kit and the renderer passes only what the kit names.
func TestTheModuleContractKitAdapterFailsOnUnrelatedErrors(t *testing.T) {
	verdict, past := kitVerdict(nil, errors.New("read the workspace configurations: disk"))
	require.Error(t, verdict, "an unrelated failure must fail the kit")
	require.False(t, past)
	verdict, past = kitVerdict(nil, fmt.Errorf("%w: tenancy", modulecontract.ErrInvalid))
	require.ErrorIs(t, verdict, modulecontract.ErrInvalid, "the reader's refusal reaches the kit as is")
	require.False(t, past)
	verdict, past = kitVerdict(nil, fmt.Errorf("%w: module shop declares the destinations chat-http (chat/http)", errUnrenderedDestination))
	require.NoError(t, verdict, "the derivation's named refusal past the reader is the reader's acceptance")
	require.True(t, past)
	verdict, past = kitVerdict(nil, nil)
	require.Error(t, verdict, "a success deriving nothing is not acceptance")
	require.False(t, past)
	verdict, past = kitVerdict(&solutionhost.Authority{Principal: "shop"}, nil)
	require.NoError(t, verdict)
	require.False(t, past)
}

// writeCalleePackages composes the two modules the kit's calls fixture calls:
// documents, whose protobuf contract marks ResolveEvidence and
// ReadDocumentExcerpt, and model-gateway, whose OpenAPI contract marks its two
// routes.
func writeCalleePackages(t *testing.T, root string) {
	t.Helper()
	read := &hopv1.Policy{Requires: []*hopv1.Requirement{{ResourceKind: "documents", Actions: []string{"read"}}, {ResourceKind: "documents.collection", Actions: []string{"read"}}}}
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String("DocumentsService")}
	for _, name := range []string{"ResolveEvidence", "ReadDocumentExcerpt"} {
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(".documents.v1.Empty"), OutputType: proto.String(".documents.v1.Empty"), Options: &descriptorpb.MethodOptions{}}
		proto.SetExtension(m.Options, hopv1.E_Policy, read)
		service.Method = append(service.Method, m)
	}
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("documents/v1/api.proto"), Package: proto.String("documents.v1"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}}, Service: []*descriptorpb.ServiceDescriptorProto{service},
	}}})
	require.NoError(t, err)
	writeCallee(t, filepath.Join(root, "modules", "documents"), "documents", "grpc", "protobuf", "documents.v1", "contract.binpb", set)
	openapi := []byte(`{"openapi":"3.1.0","info":{"title":"ModelService"},"paths":{
		"/v1/model/profiles":{"get":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"modelservice.profiles","actions":["read"]}]}}},
		"/v1/model/profiles/{name}:generate":{"post":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"modelservice.profiles","actions":["invoke"]}]}}}}}`)
	writeCallee(t, filepath.Join(root, "modules", "model-gateway"), "model-gateway", "rest", "openapi", "ModelService", "openapi.json", openapi)
}

// writeCallee writes one composed module publishing one contract.
func writeCallee(t *testing.T, dir, name, endpoint, kind, pkg, file string, contract []byte) {
	t.Helper()
	path := filepath.Join("contracts", "api", "api", endpoint, file)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "services"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, path), contract, 0o644))
	sum := sha256.Sum256(contract)
	require.NoError(t, os.WriteFile(filepath.Join(dir, resources.ModuleConfigurationName), []byte("kind: module\nname: "+name+"\nservices: []\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "module.package.codefly.yaml"), []byte(`schema: codefly/module-package/v2
kind: module-package
id: acme/`+name+`
version: 0.1.0
minimum-codefly-version: ">=0.3.32 <1.0.0"
artifact-roots:
  - contracts
  - services
services:
  - name: api
    endpoints: [`+endpoint+`]
    api-contracts:
      - endpoint: `+endpoint+`
        kind: `+kind+`
        package: `+pkg+`
        path: `+filepath.ToSlash(path)+`
        digest: sha256:`+hex.EncodeToString(sum[:])+`
contracts:
  composition: ">=2.0.0 <3.0.0"
`), 0o644))
}
