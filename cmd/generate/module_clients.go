package generate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// EndpointClientsConfig is the optional `clients:` block of one
// interface.endpoints[] entry in module.codefly.yaml. Core's module loader
// does not declare it (a plain YAML decode ignores it), so it is side-read
// here, the same way librarystore reads workspace.codefly.yaml's
// libraries.publish block.
//
//	interface:
//	  endpoints:
//	    - service: accounts
//	      endpoint: connect
//	      clients:
//	        languages: [go, typescript, python]   # default: every language the contract kind supports
//	        services: [AuditService, WebhookService] # protobuf only; default: the whole contract
//	        publish: false                         # opt this endpoint out of client publishing
type EndpointClientsConfig struct {
	Publish   *bool    `yaml:"publish"`
	Languages []string `yaml:"languages"`
	Services  []string `yaml:"services"`
}

// LoadModuleClientsConfig reads every interface endpoint's clients: block from
// the module's manifest, keyed by "service/endpoint". Endpoints without a
// block are absent from the map.
func LoadModuleClientsConfig(moduleDir string) (map[string]EndpointClientsConfig, error) {
	path := filepath.Join(moduleDir, resources.ModuleConfigurationName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		Interface struct {
			Endpoints []struct {
				Service  string                 `yaml:"service"`
				Endpoint string                 `yaml:"endpoint"`
				Clients  *EndpointClientsConfig `yaml:"clients"`
			} `yaml:"endpoints"`
		} `yaml:"interface"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]EndpointClientsConfig{}
	for _, endpoint := range doc.Interface.Endpoints {
		if endpoint.Clients == nil {
			continue
		}
		out[endpoint.Service+"/"+endpoint.Endpoint] = *endpoint.Clients
	}
	return out, nil
}

// ModuleClientPlan is one client library a module publishes: the catalog
// endpoint it binds, the languages it ships in, and the protobuf services
// its facade is restricted to (nil: the whole contract).
type ModuleClientPlan struct {
	// Name is the library name, <module>-<service>-client — the same default
	// `generate client` uses, so a library published by hand before and one
	// published by `publish clients` land under one identity.
	Name      string
	Endpoint  composition.APIContractEndpoint
	Languages []languages.Language
	Services  []string
}

// DefaultClientLanguages returns the languages a contract kind can be
// generated into: python has no OpenAPI client generator.
func DefaultClientLanguages(kind string) []languages.Language {
	if kind == composition.APIContractKindOpenAPI {
		return []languages.Language{languages.GO, languages.TYPESCRIPT}
	}
	return []languages.Language{languages.GO, languages.TYPESCRIPT, languages.PYTHON}
}

// PlanModuleClients derives the client libraries a module publishes from its
// exported contract catalog (contracts/api, the published unit whose digests
// the package manifest pins) and the endpoints' clients: configuration. It
// reads nothing but files, so `--check` and `--dry-run` need no toolchain.
// requested, when non-empty, restricts every plan to those languages.
func PlanModuleClients(module *resources.Module, catalog *composition.APIContractCatalog, config map[string]EndpointClientsConfig, requested []languages.Language) ([]ModuleClientPlan, error) {
	for key := range config {
		found := false
		for i := range catalog.Endpoints {
			if catalog.Endpoints[i].Service+"/"+catalog.Endpoints[i].Endpoint == key {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("interface endpoint %s declares clients: but exports no API contract; run `codefly generate contracts %s` or drop the block", key, module.Name)
		}
	}
	var plans []ModuleClientPlan
	for i := range catalog.Endpoints {
		endpoint := catalog.Endpoints[i]
		cfg := config[endpoint.Service+"/"+endpoint.Endpoint]
		if cfg.Publish != nil && !*cfg.Publish {
			continue
		}
		langs := DefaultClientLanguages(endpoint.Kind)
		if len(cfg.Languages) > 0 {
			langs = langs[:0]
			for _, raw := range cfg.Languages {
				lang := languages.FromString(strings.TrimSpace(raw))
				if lang == languages.NotSupported {
					return nil, fmt.Errorf("interface endpoint %s/%s: client language %q is not supported", endpoint.Service, endpoint.Endpoint, raw)
				}
				if !containsLanguage(DefaultClientLanguages(endpoint.Kind), lang) {
					return nil, fmt.Errorf("interface endpoint %s/%s: %s contracts have no %s client generator", endpoint.Service, endpoint.Endpoint, endpoint.Kind, lang)
				}
				if !containsLanguage(langs, lang) {
					langs = append(langs, lang)
				}
			}
		}
		if len(requested) > 0 {
			kept := langs[:0]
			for _, lang := range langs {
				if containsLanguage(requested, lang) {
					kept = append(kept, lang)
				}
			}
			langs = kept
		}
		if len(langs) == 0 {
			continue
		}
		var services []string
		if len(cfg.Services) > 0 {
			if endpoint.Kind != composition.APIContractKindProtobuf {
				return nil, fmt.Errorf("interface endpoint %s/%s: clients.services only applies to protobuf contracts", endpoint.Service, endpoint.Endpoint)
			}
			declared := map[string]bool{}
			for _, svc := range endpoint.Services {
				declared[svc.Name] = true
			}
			for _, name := range cfg.Services {
				name = strings.TrimSpace(name)
				if !declared[name] {
					return nil, fmt.Errorf("interface endpoint %s/%s: clients.services names %q, which the exported contract (package %s) does not declare", endpoint.Service, endpoint.Endpoint, name, endpoint.Package)
				}
				services = append(services, name)
			}
			sort.Strings(services)
		}
		plans = append(plans, ModuleClientPlan{
			Name:      fmt.Sprintf("%s-%s-client", module.Name, endpoint.Service),
			Endpoint:  endpoint,
			Languages: langs,
			Services:  services,
		})
	}
	return plans, nil
}

// BuildModuleClientEntry turns a plan into the contract entry
// generators.GenerateClientLibrary consumes. It rebuilds the contract from
// the live service — the only source that carries the .proto files a
// services subset needs (see resolvedContractSource.protoSources) — and
// refuses to proceed when that build does not reproduce the catalog's
// digest: the on-disk export is then stale and `generate contracts` must run
// first, so a published client never binds a contract the package does not
// pin.
func BuildModuleClientEntry(ctx context.Context, workspace *resources.Workspace, module *resources.Module, catalog *composition.APIContractCatalog, plan *ModuleClientPlan) (*generators.ContractEntry, error) {
	service, err := module.LoadServiceFromName(ctx, plan.Endpoint.Service)
	if err != nil {
		return nil, fmt.Errorf("cannot load service %s: %w", plan.Endpoint.Service, err)
	}
	loaded, err := generators.LoadServiceEndpoints(ctx, workspace, module, service)
	if err != nil {
		return nil, fmt.Errorf("cannot load endpoints for %s/%s: %w", module.Name, service.Name, err)
	}
	var live *basev0.Endpoint
	for _, endpoint := range loaded {
		if endpoint.Name == plan.Endpoint.Endpoint {
			live = endpoint
			break
		}
	}
	if live == nil {
		return nil, fmt.Errorf("service %s does not report endpoint %q at Builder.Load", service.Name, plan.Endpoint.Endpoint)
	}
	contractBytes, built, protoSources, err := buildLocalContractEndpoint(ctx, service, live)
	if err != nil {
		return nil, fmt.Errorf("cannot build contract for %s/%s/%s: %w", module.Name, service.Name, live.Name, err)
	}
	if built.Digest != plan.Endpoint.Digest {
		return nil, fmt.Errorf("contracts/api for %s/%s is stale (catalog digest %s, source builds %s); run `codefly generate contracts %s` and commit the result before publishing clients",
			service.Name, live.Name, plan.Endpoint.Digest, built.Digest, module.Name)
	}
	return &generators.ContractEntry{
		Endpoint:            plan.Endpoint,
		ContractBytes:       contractBytes,
		PackageID:           catalog.Package,
		PackageVersion:      catalog.Version,
		ModuleName:          module.Name,
		FacadeModuleDefault: generators.DefaultFacadeModuleName(&plan.Endpoint),
		ProtoSources:        protoSources,
		Services:            plan.Services,
	}, nil
}

func containsLanguage(list []languages.Language, lang languages.Language) bool {
	for _, candidate := range list {
		if candidate == lang {
			return true
		}
	}
	return false
}
