package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// THE EDGE ROUTES WHAT A MODULE DECLARES.
//
// A composition's browser edge sends a page's call to the module that serves
// its procedure. Which procedures a module serves is what the module
// publishes — its API catalog, the source of the hop's routes — and was, until
// this group, a list each composition typed again (`<MODULE>_RPC` in the edge
// group), so a composition that forgot a service answered "no module serves"
// for a procedure its module publishes.
//
// edge-routes is that list, generated: for every module the edge mounts
// (edge/MODULES) that is published through the hop (hop/MODULES), the
// procedures its own contracts declare on a Connect listener, the transport a
// page speaks and the hop's HTTP face serves. It carries no address: where a
// module's face is stays the composition's `<MODULE>_CONNECT`, an ordinary
// endpoint reference.
//
// The catalog does not say which service a page calls and which only another
// module does (robin's execution and sandbox services are published like its
// chat service), so the group holds every one of them and the hop decides
// admission, as it does for any caller. Two things are left out because the
// catalog and the module contract do say so:
//
//   - a FOREIGN METHOD: another module's protocol, served to that module.
//     Several mounted modules may serve the same one, and a page's path names
//     no module to choose between them;
//   - a module the edge mounts that is NOT behind the hop (a solution's own
//     backend at its REST face): it publishes nothing through the hop.
//
// hop-routes is untouched by this group: declaring hop-routes is still what
// makes a service the hop (gitops/hop.go), and declaring edge-routes makes a
// service nothing but a reader of this document.
const (
	edgeRoutesGroup = "edge-routes"
	// edgeRoutesKey is the one value of that group: the document below.
	edgeRoutesKey = "ROUTES"
	// EdgeRoutesSchema identifies the document. The edge decodes it
	// (authority, modules/authority/services/edge/code/routes.go) and holds
	// the same bytes in its tests.
	EdgeRoutesSchema = "codefly.dev/edge-routes/v1"
	// edgeGroup is the authored group a composition states its edge's inputs
	// in; its MODULES value names the modules the edge mounts.
	edgeGroup = "edge"
)

// edgeRoutes is the generated document: per mounted module published through
// the hop, the procedures it serves over Connect.
type edgeRoutes struct {
	Schema  string             `json:"schema"`
	Modules []edgeModuleRoutes `json:"modules"`
}

type edgeModuleRoutes struct {
	// Audience is the module's principal, the name the edge mounts it under.
	Audience string `json:"audience"`
	// Procedures are full protobuf procedures, /package.Service/Method, sorted.
	Procedures []string `json:"procedures"`
}

// generatedEdgeRoutes generates the edge-routes group for a workspace a
// service of which declares it, and nothing otherwise. The declaring service
// always receives a document, naming no module where the edge mounts none
// behind the hop: Core's reader refuses a declared group that resolves to
// nothing, and an edge handed no document knows it runs under a CLI that
// generates none.
func generatedEdgeRoutes(ctx context.Context, workspace *resources.Workspace, configs []*basev0.Configuration) (*basev0.Configuration, error) {
	var mounted, fronted string
	for _, info := range workspaceConfigurationInfos(configs) {
		if info.GetName() == edgeRoutesGroup {
			return nil, fmt.Errorf("%s is generated from published API catalogs; remove its authored configuration", edgeRoutesGroup)
		}
		for _, value := range info.GetConfigurationValues() {
			switch {
			case info.GetName() == edgeGroup && value.GetKey() == "MODULES":
				mounted = value.GetValue()
			case info.GetName() == "hop" && value.GetKey() == "MODULES":
				fronted = value.GetValue()
			}
		}
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(services, func(s *resources.Service) bool {
		return slices.Contains(s.WorkspaceConfigurationDependencies, edgeRoutesGroup)
	}) {
		return nil, nil
	}
	behindHop := listedModules(fronted)
	routes := edgeRoutes{Schema: EdgeRoutesSchema, Modules: []edgeModuleRoutes{}}
	for _, name := range listedModules(mounted) {
		if !slices.Contains(behindHop, name) {
			continue
		}
		module, loadErr := workspace.LoadModuleFromName(ctx, name)
		if loadErr != nil {
			return nil, fmt.Errorf("edge routes for %s: %w", name, loadErr)
		}
		endpoints, contract, routeErr := publishedEndpoints(ctx, module)
		if routeErr != nil {
			return nil, fmt.Errorf("edge routes for %s: %w", name, routeErr)
		}
		entry := edgeModuleRoutes{Audience: contract.Principal, Procedures: []string{}}
		for i := range endpoints {
			if endpoints[i].Transport == "connect" {
				entry.Procedures = append(entry.Procedures, endpoints[i].Methods...)
			}
		}
		slices.Sort(entry.Procedures)
		entry.Procedures = slices.Compact(entry.Procedures)
		routes.Modules = append(routes.Modules, entry)
	}
	raw, err := json.Marshal(routes)
	if err != nil {
		return nil, err
	}
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: edgeRoutesGroup, ConfigurationValues: []*basev0.ConfigurationValue{{Key: edgeRoutesKey, Value: string(raw)}},
	}}}, nil
}

// listedModules reads a comma-separated MODULES value: sorted, each name once,
// blanks dropped, as the edge itself reads its list.
func listedModules(listed string) []string {
	var names []string
	for _, name := range strings.Split(listed, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
