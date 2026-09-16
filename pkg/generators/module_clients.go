package generators

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"gopkg.in/yaml.v3"
)

const (
	ModuleClientsConfigFileName = "clients.codefly.yaml"
	ModuleClientsConfigSchema   = "codefly/module-clients-config/v1"
)

type EndpointClientsConfig struct {
	Service   string   `yaml:"service"`
	Endpoint  string   `yaml:"endpoint"`
	Publish   *bool    `yaml:"publish,omitempty"`
	Languages []string `yaml:"languages,omitempty"`
	Services  []string `yaml:"services,omitempty"`
}

type moduleClientsConfig struct {
	Schema    string                  `yaml:"schema"`
	Endpoints []EndpointClientsConfig `yaml:"endpoints,omitempty"`
}

// LoadModuleClientsConfig reads the optional, publish-owned client policy.
// Keeping it outside module.codefly.yaml's generated interface block prevents
// module synchronization from replacing product-owned publication policy.
func LoadModuleClientsConfig(moduleDir string) (map[string]EndpointClientsConfig, error) {
	path := filepath.Join(moduleDir, ModuleClientsConfigFileName)
	file, err := os.Open(path) //nolint:gosec // moduleDir is the caller-selected module.
	if errors.Is(err, os.ErrNotExist) {
		return map[string]EndpointClientsConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var doc moduleClientsConfig
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: multiple YAML documents are not supported", path)
	}
	if doc.Schema != ModuleClientsConfigSchema {
		return nil, fmt.Errorf("%s: unsupported schema %q (want %s)", path, doc.Schema, ModuleClientsConfigSchema)
	}
	out := make(map[string]EndpointClientsConfig, len(doc.Endpoints))
	for _, endpoint := range doc.Endpoints {
		endpoint.Service = strings.TrimSpace(endpoint.Service)
		endpoint.Endpoint = strings.TrimSpace(endpoint.Endpoint)
		if endpoint.Service == "" || endpoint.Endpoint == "" {
			return nil, fmt.Errorf("%s: every endpoint requires service and endpoint", path)
		}
		key := endpoint.Service + "/" + endpoint.Endpoint
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("%s: endpoint %s is configured twice", path, key)
		}
		out[key] = endpoint
	}
	return out, nil
}

type ModuleClientPlan struct {
	Name      string
	Endpoint  composition.APIContractEndpoint
	Languages []languages.Language
	Services  []string
}

func DefaultClientLanguages(kind string) []languages.Language {
	if kind == composition.APIContractKindOpenAPI {
		return []languages.Language{languages.GO, languages.TYPESCRIPT}
	}
	return []languages.Language{languages.GO, languages.TYPESCRIPT, languages.PYTHON}
}

func PlanModuleClients(moduleName string, catalog *composition.APIContractCatalog, config map[string]EndpointClientsConfig, requested []languages.Language) ([]ModuleClientPlan, error) {
	for key := range config {
		found := false
		for i := range catalog.Endpoints {
			if catalog.Endpoints[i].Service+"/"+catalog.Endpoints[i].Endpoint == key {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%s configures clients for an API contract the module does not export; run `codefly generate contracts %s` or drop the entry", key, moduleName)
		}
	}

	plans := make([]ModuleClientPlan, 0, len(catalog.Endpoints))
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
					return nil, fmt.Errorf("endpoint %s/%s: client language %q is not supported", endpoint.Service, endpoint.Endpoint, raw)
				}
				if !containsLanguage(DefaultClientLanguages(endpoint.Kind), lang) {
					return nil, fmt.Errorf("endpoint %s/%s: %s contracts have no %s client generator", endpoint.Service, endpoint.Endpoint, endpoint.Kind, lang)
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
				return nil, fmt.Errorf("endpoint %s/%s: services only applies to protobuf contracts", endpoint.Service, endpoint.Endpoint)
			}
			declared := map[string]bool{}
			for _, svc := range endpoint.Services {
				declared[svc.Name] = true
			}
			seen := map[string]bool{}
			for _, name := range cfg.Services {
				name = strings.TrimSpace(name)
				if !declared[name] {
					return nil, fmt.Errorf("endpoint %s/%s: services names %q, which the exported contract (package %s) does not declare", endpoint.Service, endpoint.Endpoint, name, endpoint.Package)
				}
				if !seen[name] {
					services = append(services, name)
					seen[name] = true
				}
			}
			sort.Strings(services)
		}
		plans = append(plans, ModuleClientPlan{
			Name:      fmt.Sprintf("%s-%s-%s-client", moduleName, endpoint.Service, endpoint.Endpoint),
			Endpoint:  endpoint,
			Languages: langs,
			Services:  services,
		})
	}
	return plans, nil
}

// BuildModuleClientEntry reads exactly the committed package contract that is
// being published. Callers validate the package manifest, catalog and artifact
// digests before invoking it, so generated clients cannot diverge from the
// module package under the same version.
func BuildModuleClientEntry(ctx context.Context, moduleDir, moduleName string, catalog *composition.APIContractCatalog, plan *ModuleClientPlan) (*ContractEntry, error) {
	contractBytes, err := ReadCatalogContractBytes(moduleDir, &plan.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("read contract for %s/%s: %w", plan.Endpoint.Service, plan.Endpoint.Endpoint, err)
	}
	var protoSources []coreproto.Source
	if plan.Endpoint.Kind == composition.APIContractKindProtobuf {
		protoDir := filepath.Join(filepath.Dir(filepath.Join(moduleDir, filepath.FromSlash(plan.Endpoint.Path))), "proto")
		protoSources, err = collectPublishedProtoSources(ctx, protoDir)
		if err != nil {
			return nil, fmt.Errorf("read proto sources for %s/%s: %w", plan.Endpoint.Service, plan.Endpoint.Endpoint, err)
		}
	}
	return &ContractEntry{
		Endpoint:            plan.Endpoint,
		ContractBytes:       contractBytes,
		PackageID:           catalog.Package,
		PackageVersion:      catalog.Version,
		ModuleName:          moduleName,
		FacadeModuleDefault: DefaultFacadeModuleName(&plan.Endpoint),
		ProtoSources:        protoSources,
		Services:            plan.Services,
	}, nil
}

func collectPublishedProtoSources(ctx context.Context, protoDir string) ([]coreproto.Source, error) {
	var sources []coreproto.Source
	err := filepath.WalkDir(protoDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".proto" {
			return nil
		}
		rel, err := filepath.Rel(protoDir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path) //nolint:gosec // walked beneath the validated contract directory.
		if err != nil {
			return err
		}
		sources = append(sources, coreproto.Source{Path: filepath.ToSlash(rel), Content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no .proto files under %s", protoDir)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func containsLanguage(list []languages.Language, lang languages.Language) bool {
	for _, candidate := range list {
		if candidate == lang {
			return true
		}
	}
	return false
}
