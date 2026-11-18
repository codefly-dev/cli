package generate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const ownerInterface = `kind: interface
publisher: example.dev
name: note-owner
version: 0.1.0
type: connect
protobuf:
    package: notes.v1
    services:
        - name: OwnerService
          methods: [ListKinds]
`

// An inventory row that declares its service the protocol of an interface
// other modules implement is exempt from being served here (core's
// ProtobufCoverage). The exemption is only as true as the definition it
// names: the module publishes that definition under interfaces/, and it lists
// exactly that service. A row naming a definition nobody published, another
// version, or a definition of some other service, is refused by name.
func TestAnInterfaceRowNamesADefinitionThatListsItsService(t *testing.T) {
	ctx := context.Background()
	row := func(fullName, identity string) contractSurfaces {
		return contractSurfaces{"grpc": {
			{Name: "NotesService", FullName: "notes.v1.NotesService", Procedures: []string{"/notes.v1.NotesService/Get"}},
			{Name: "OwnerService", FullName: fullName, Interface: identity},
		}}
	}
	for _, tc := range []struct {
		name       string
		definition string
		surfaces   contractSurfaces
		refusal    string
	}{
		{name: "the published definition lists the service", definition: ownerInterface,
			surfaces: row("notes.v1.OwnerService", "example.dev/note-owner@0.1.0")},
		{name: "a row with no interface asks for no definition",
			surfaces: contractSurfaces{"grpc": {{Name: "NotesService", FullName: "notes.v1.NotesService"}}}},
		{name: "no definition is published",
			surfaces: row("notes.v1.OwnerService", "example.dev/note-owner@0.1.0"),
			refusal:  "service notes.v1.OwnerService on endpoint grpc: interface example.dev/note-owner@0.1.0: module notes publishes no definition of it under interfaces/"},
		{name: "another version is published", definition: ownerInterface,
			surfaces: row("notes.v1.OwnerService", "example.dev/note-owner@0.2.0"),
			refusal:  "module notes publishes no definition of it under interfaces/ (it publishes example.dev/note-owner@0.1.0)"},
		{name: "the definition lists another service", definition: ownerInterface,
			surfaces: row("notes.v1.OtherService", "example.dev/note-owner@0.1.0"),
			refusal:  "service notes.v1.OtherService on endpoint grpc is declared as interface example.dev/note-owner@0.1.0, whose definition lists notes.v1.OwnerService and not it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := &resources.Module{Name: "notes"}
			module.WithDir(t.TempDir())
			if tc.definition != "" {
				writeFixtureFile(t, filepath.Join(module.Dir(), "interfaces", "note-owner", resources.InterfaceConfigurationName), []byte(tc.definition))
			}
			err := checkInterfaceRows(ctx, module, tc.surfaces)
			if tc.refusal == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.refusal)
		})
	}
}

// A capability definition has no protobuf service to be the protocol of.
func TestAnInterfaceRowRefusesADefinitionWithNoProtobufSurface(t *testing.T) {
	module := &resources.Module{Name: "notes"}
	module.WithDir(t.TempDir())
	writeFixtureFile(t, filepath.Join(module.Dir(), "interfaces", "cache", resources.InterfaceConfigurationName), []byte(`kind: interface
publisher: example.dev
name: cache
version: 0.3.0
type: capability
capability:
    configuration: connection
    keys:
        - name: url
`))
	err := checkInterfaceRows(context.Background(), module, contractSurfaces{"grpc": {
		{Name: "OwnerService", FullName: "notes.v1.OwnerService", Interface: "example.dev/cache@0.3.0"},
	}})
	require.ErrorContains(t, err, "is a capability interface and lists no protobuf service")
}

// The command refuses such a row where the inventory is read, before any
// builder is loaded.
func TestGenerateContractsRefusesAnInterfaceRowWithNoPublishedDefinition(t *testing.T) {
	service := &resources.Service{Name: "owner", Version: "0.1.0",
		Agent:     &resources.Agent{Kind: resources.ServiceAgent, Name: "go", Publisher: "codefly.dev", Version: "0.0.48"},
		Endpoints: []*resources.Endpoint{{Name: "grpc", API: "grpc"}},
	}
	root, moduleDir := saveFixtureWorkspace(t, context.Background(), "notes",
		&resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{
			{Service: "owner", Endpoint: "grpc", Visibility: resources.VisibilityInternal},
		}}, service)
	writeFixtureFile(t, filepath.Join(service.Dir(), "proto/buf.yaml"), []byte("version: v2\n"))
	writeFixtureFile(t, filepath.Join(service.Dir(), "surfaces.json"), []byte(`{"grpc": [
  {"name":"NotesService","fullName":"notes.v1.NotesService","procedures":["/notes.v1.NotesService/Get"]},
  {"name":"OwnerService","fullName":"notes.v1.OwnerService","interface":"example.dev/note-owner@0.1.0","procedures":[]}
]}`))
	fixtureSurfacePath(t, moduleDir, service.Name, "surfaces.json")
	t.Chdir(root)
	resetContractsFlags(t)
	err := ContractsCmd.RunE(ContractsCmd, []string{"notes"})
	require.ErrorContains(t, err, "module notes: service notes.v1.OwnerService on endpoint grpc: interface example.dev/note-owner@0.1.0: module notes publishes no definition of it under interfaces/")
}
