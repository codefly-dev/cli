package generators

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"gopkg.in/yaml.v3"
)

// ClientRelease is the version-only part of an existing generated library.
// Paths lets the release owner restore every write if its release gate fails.
// Contract bytes, generator provenance and unrelated sources stay put.
type ClientRelease struct {
	Paths        []string
	dir          string
	document     yaml.Node
	sources      []librarySource
	catalogs     map[int]*composition.APIContractCatalog
	catalogPaths map[int]string
	langs        []generatedLanguageExport
}

// ClientReleasePackage is the committed producer whose contract a client binds.
type ClientReleasePackage struct {
	Directory string
	Catalog   *composition.APIContractCatalog
}

// PlanClientRelease selects sources owned by the releasing packages. The owner
// supplies its committed catalogs and checks every path before it is read, using
// the same containment/tracking rules as the rest of its release. No file is
// written during planning; a stale source must be regenerated before release.
func PlanClientRelease(dir string, packages map[string]ClientReleasePackage, checkPath func(string) error) (*ClientRelease, error) {
	manifest := filepath.Join(dir, resources.LibraryConfigurationName)
	if err := checkPath(manifest); err != nil {
		return nil, err
	}
	sources, err := readExistingLibrarySources(dir)
	if err != nil {
		return nil, err
	}
	matched := false
	for i := range sources {
		_, ok := packages[sources[i].Package]
		matched = matched || ok
	}
	if !matched {
		return nil, nil
	}
	plan := &ClientRelease{dir: dir, sources: sources, catalogs: map[int]*composition.APIContractCatalog{}, catalogPaths: map[int]string{}}
	raw, err := os.ReadFile(manifest) //nolint:gosec // checked release path
	if err != nil {
		return nil, err
	}
	if err = yaml.Unmarshal(raw, &plan.document); err != nil {
		return nil, err
	}
	plan.Paths = append(plan.Paths, manifest)
	for i := range sources {
		if producer, releasing := packages[sources[i].Package]; releasing {
			if err = plan.addSource(i, producer, checkPath); err != nil {
				return nil, err
			}
		}
	}
	if len(sources) == 1 {
		plan.langs, err = readExistingLanguageExports(dir)
		if err != nil {
			return nil, err
		}
		for _, lang := range plan.langs {
			if !filepath.IsLocal(lang.Path) {
				return nil, fmt.Errorf("client language path %q escapes its library", lang.Path)
			}
			if path, ok := packageManifest(languages.Language(lang.Name), filepath.Join(dir, lang.Path)); ok {
				if err := checkPath(path); err != nil {
					return nil, err
				}
				if err := checkPackageVersionStampable(languages.Language(lang.Name), filepath.Dir(path)); err != nil {
					return nil, err
				}
				plan.Paths = append(plan.Paths, path)
			}
		}
	}
	return plan, nil
}

func (plan *ClientRelease) addSource(i int, producer ClientReleasePackage, checkPath func(string) error) error {
	source := &plan.sources[i]
	owner := producer.Catalog
	manifest := filepath.Join(plan.dir, resources.LibraryConfigurationName)
	if owner == nil || owner.Package != source.Package || owner.Version != source.Version || !sourceMatchesCatalog(source, owner) {
		return fmt.Errorf("%s source %s/%s is stale; regenerate the client before release", manifest, source.Service, source.Endpoint)
	}
	contractDir := filepath.Join(plan.dir, "contract")
	if len(plan.sources) > 1 {
		entry := ContractEntry{ModuleName: source.Module, Endpoint: composition.APIContractEndpoint{Service: source.Service, Endpoint: source.Endpoint}}
		name := contractEntryDirName(&entry)
		if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("invalid generated contract directory %q", name)
		}
		contractDir = filepath.Join(contractDir, name)
	}
	path := filepath.Join(contractDir, "catalog.codefly.json")
	if err := checkPath(path); err != nil {
		return err
	}
	catalog, err := readClientReleaseCatalog(path)
	if err != nil {
		return err
	}
	if catalog.Package != source.Package || catalog.Version != source.Version || len(catalog.Endpoints) != 1 || catalog.Endpoints[0].Service != source.Service || catalog.Endpoints[0].Endpoint != source.Endpoint {
		return fmt.Errorf("%s disagrees with its library source; regenerate the client before release", path)
	}
	endpoint := &catalog.Endpoints[0]
	if err := checkPath(filepath.Join(contractDir, filepath.FromSlash(endpoint.Path))); err != nil {
		return err
	}
	if err := verifyReleaseClientContract(contractDir, source, producer, endpoint, checkPath); err != nil {
		return err
	}
	plan.catalogs[i], plan.catalogPaths[i] = catalog, path
	plan.Paths = append(plan.Paths, path)
	return nil
}

func sourceMatchesCatalog(source *librarySource, catalog *composition.APIContractCatalog) bool {
	for i := range catalog.Endpoints {
		endpoint := &catalog.Endpoints[i]
		if endpoint.Service == source.Service && endpoint.Endpoint == source.Endpoint {
			return endpoint.Digest == source.ContractDigest
		}
	}
	return false
}

// A copied protobuf contract can be a generator-narrowed subset: its digest
// then differs from the source digest. Reproduce that existing transformation
// from the committed producer rather than trusting a self-consistent copy.
func verifyReleaseClientContract(dir string, source *librarySource, producer ClientReleasePackage, copied *composition.APIContractEndpoint, checkPath func(string) error) error {
	if !slices.Equal(source.Services, protobufServiceNames(copied.Services)) {
		return fmt.Errorf("copied client service selection disagrees with its source; regenerate the client before release")
	}
	var endpoint composition.APIContractEndpoint
	for i := range producer.Catalog.Endpoints {
		candidate := &producer.Catalog.Endpoints[i]
		if candidate.Service == source.Service && candidate.Endpoint == source.Endpoint {
			endpoint = *candidate
			break
		}
	}
	if err := checkPath(filepath.Join(producer.Directory, filepath.FromSlash(endpoint.Path))); err != nil {
		return err
	}
	original, err := ReadCatalogContractBytes(producer.Directory, &endpoint)
	if err != nil {
		return err
	}
	actual, err := ReadCatalogContractBytes(dir, copied)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, original) {
		if len(source.Services) == 0 || endpoint.Kind != composition.APIContractKindProtobuf {
			return fmt.Errorf("copied client contract differs from its producer; regenerate the client before release")
		}
		entries, narrowErr := narrowEntries([]ContractEntry{{Endpoint: endpoint, ContractBytes: original, Services: source.Services}})
		if narrowErr != nil {
			return narrowErr
		}
		if !bytes.Equal(actual, entries[0].ContractBytes) {
			return fmt.Errorf("copied client contract differs from its selected producer services; regenerate the client before release")
		}
		endpoint = entries[0].Endpoint
	}
	endpoint.Path = "contract.binpb"
	if endpoint.Kind == composition.APIContractKindOpenAPI {
		endpoint.Path = "openapi.json"
	}
	endpoint.Digest = composition.APIContractDigest(actual)
	expected := &composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: source.Package, Version: source.Version, Endpoints: []composition.APIContractEndpoint{endpoint}}
	got := *expected
	got.Endpoints = []composition.APIContractEndpoint{*copied}
	expectedBytes, err := expected.CanonicalBytes()
	if err != nil {
		return err
	}
	actualBytes, err := got.CanonicalBytes()
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedBytes, actualBytes) {
		return fmt.Errorf("copied client catalog differs from its producer; regenerate the client before release")
	}
	return nil
}

func readClientReleaseCatalog(path string) (*composition.APIContractCatalog, error) {
	data, err := os.ReadFile(path) //nolint:gosec // checked release path
	if err != nil {
		return nil, err
	}
	catalog := &composition.APIContractCatalog{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(catalog); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s has trailing JSON", path)
	}
	return catalog, catalog.Validate()
}

// Write applies versions supplied by the updated owner package manifests.
// An aggregate keeps its own top-level and language versions, exactly as generate.
func (plan *ClientRelease) Write(ctx context.Context, versions map[string]string) error {
	root := plan.document.Content[0]
	sources := libraryField(root, "sources")
	if sources == nil || len(sources.Content) != len(plan.sources) {
		return fmt.Errorf("invalid library sources")
	}
	for i := range plan.sources {
		catalog, releasing := plan.catalogs[i]
		if !releasing {
			continue
		}
		version := versions[plan.sources[i].Package]
		if version == "" {
			return fmt.Errorf("no released version for %s", plan.sources[i].Package)
		}
		catalog.Version = version
		if err := catalog.Validate(); err != nil {
			return err
		}
		data, err := catalog.CanonicalBytes()
		if err != nil {
			return err
		}
		if err := shared.WriteFileAtomic(ctx, plan.catalogPaths[i], data, 0o644); err != nil {
			return err
		}
		field := libraryField(sources.Content[i], "version")
		if field == nil {
			return fmt.Errorf("library source has no version")
		}
		field.Value = version
		if len(plan.sources) == 1 {
			field := libraryField(root, "version")
			if field == nil {
				return fmt.Errorf("library has no version")
			}
			field.Value = version
			for _, lang := range plan.langs {
				if err := stampPackageVersion(languages.Language(lang.Name), filepath.Join(plan.dir, lang.Path), version); err != nil {
					return err
				}
			}
		}
	}
	data, err := yaml.Marshal(&plan.document)
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, filepath.Join(plan.dir, resources.LibraryConfigurationName), data, 0o644)
}

func libraryField(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
