package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost/cell"
	"github.com/codefly-dev/core/solutionhost/modulecontract"
	"github.com/stretchr/testify/require"
)

// TestTheModuleContractKitRunsThroughTheRender drives core's module-contract
// kit through this repository's own entrypoint — the authority derivation,
// which reads the contract a module publishes — rather than through core's
// parser, which would prove nothing about this reader. Every fixture core
// refuses is refused here with the same sentinel and reason. Every fixture
// core accepts is read, and then either derives authority instances or is
// refused by the derivation for the ONE reason it names past the reader (a
// field the signed authority document cannot carry yet); which accepted
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
	require.NoError(t, os.WriteFile(configuration, []byte(strings.Replace(string(config), "modules:\n  - name: shop\n", "modules:\n  - name: shop\n  - name: assistant\n", 1)), 0o644))
	for rel, content := range map[string]string{
		filepath.Join("modules", "assistant", resources.ModuleConfigurationName):                     "kind: module\nname: assistant\nservices:\n  - name: api\n",
		filepath.Join("modules", "assistant", "services", "api", resources.ServiceConfigurationName): cellServiceYAML("api", "assistant") + "module-identity: true\n",
		filepath.Join("modules", "assistant", "module.package.codefly.yaml"):                         "schema: codefly/module-package/v1\nid: acme/assistant\nversion: 1.0.0\n",
		filepath.Join("configurations", "staging", "assistant.env"):                                  "MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nMODEL_BINDING=model\nEVIDENCE_AUDIENCE=documents\nEVIDENCE_RESOURCE_KIND=documents.passages\nANNOTATIONS_PREFIX=annotations\n",
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
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api"}}
	fixtures, err := modulecontract.Fixtures()
	require.NoError(t, err)
	names := map[string]string{}
	for _, fixture := range fixtures {
		names[string(fixture.Document)] = fixture.Name
	}
	var uncarried []string
	modulecontract.Run(t, func(document []byte) error {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), document, 0o600))
		instances, _, err := authorityInstancesOf(ctx, workspace, module, services, env, units)
		verdict, past := kitVerdict(instances, err)
		if past {
			uncarried = append(uncarried, names[string(document)])
		}
		return verdict
	})
	sort.Strings(uncarried)
	// The accepted fixtures declaring scope ceilings, destinations or a
	// lookup method — what the document cannot carry — and no other.
	require.Equal(t, []string{"upper-case-slot-key", "valid"}, uncarried)
}

// kitVerdict maps the renderer's verdict on a kit document to what the kit
// checks. Core's sentinels pass through: the reader refused. A contract the
// reader accepted and the derivation refused for the one reason it names past
// the reader (errUncarriedAuthority) is acceptance for the kit, reported as
// past so the test can hold the set of such fixtures. A success must have
// derived instances. Anything else is a failure the kit must see: mapping it
// to acceptance would make the run green about a name.
func kitVerdict(instances []AuthorityInstance, err error) (verdict error, past bool) {
	switch {
	case err == nil:
		if len(instances) == 0 {
			return errors.New("the contract was read and resolved but derived no authority instance"), false
		}
		return nil, false
	case errors.Is(err, modulecontract.ErrInvalid), errors.Is(err, modulecontract.ErrSchema):
		return err, false
	case errors.Is(err, errUncarriedAuthority):
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
	verdict, past = kitVerdict(nil, fmt.Errorf("%w: module shop declares scope_ceilings", errUncarriedAuthority))
	require.NoError(t, verdict, "the derivation's named refusal past the reader is the reader's acceptance")
	require.True(t, past)
	verdict, past = kitVerdict(nil, nil)
	require.Error(t, verdict, "a success deriving nothing is not acceptance")
	require.False(t, past)
	verdict, past = kitVerdict([]AuthorityInstance{{}}, nil)
	require.NoError(t, verdict)
	require.False(t, past)
}

// TestTheCellKitRunsThroughThePublisher drives core's cell kit through the one
// way this publisher reads a cell — the workspace's file, the record a render
// writes with its tree and the delivered cell alike — so a cell core refuses
// is refused here by the same name.
func TestTheCellKitRunsThroughThePublisher(t *testing.T) {
	cell.Run(t, func(document []byte) error {
		_, err := readCellFile(document)
		return err
	})
}
