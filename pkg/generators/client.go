// Package generators hosts the codegen entry points shared by the commands
// that produce a codefly library: `generate client` (one contract) and
// `sync solution-sdk` (N contracts aggregated into one library).
package generators

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"google.golang.org/protobuf/encoding/protowire"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// defaultFacadeModule is the fallback facade entry-point name — proto.GenerateClient's
// own default when Module is empty.
const defaultFacadeModule = "client"

// localPackageID is the source package/contract-entry-directory identity for
// an entry generated from a live local service rather than a composed module
// package (no producer package to name).
const localPackageID = "local"

// ContractEntry is one API contract a generated library binds: an endpoint's
// catalog entry, the raw contract bytes it points to, the package/module that
// produced it, and this entry's own generation options (a services subset, a
// facade entry-point override).
type ContractEntry struct {
	Endpoint       composition.APIContractEndpoint
	ContractBytes  []byte
	PackageID      string
	PackageVersion string
	ModuleName     string

	// FacadeModuleDefault is the facade plugins' own default entry-point name
	// for this contract (the last non-version segment of its proto package).
	FacadeModuleDefault string

	// ProtoSources, when set, carries the endpoint's original .proto files
	// instead of relying on the persisted descriptor set: proto.GenerateClient's
	// DescriptorSet path mishandles a Services subset once more than one file is
	// present (core bug: a facade plugin invoked over a multi-file descriptor
	// set reports the target service as undeclared, even though the same
	// content generates fine as Sources).
	ProtoSources []coreproto.Source

	// Services restricts this entry's facade to these protobuf service names.
	Services []string
	// As overrides this entry's facade entry-point name (default:
	// FacadeModuleDefault, else defaultFacadeModule).
	As string
}

// ClientLibraryRequest is one library-generation run: N contract entries
// generated into one output directory, sharing one go.mod/package.json/pyproject.toml
// per language.
type ClientLibraryRequest struct {
	Name       string
	Output     string
	Languages  []languages.Language
	Entries    []ContractEntry
	NoFacade   bool
	Force      bool
	GoModule   string
	NpmPackage string
}

// generatedLanguageExport is one languages: entry of library.codefly.yaml.
type generatedLanguageExport struct {
	Name    string
	Path    string
	Exports []string
}

// librarySource is one sources: entry of library.codefly.yaml — the schema
// both `generate client` (always one element) and `sync solution-sdk`
// (one or more) write.
type librarySource struct {
	Package        string   `yaml:"package"`
	Version        string   `yaml:"version"`
	Module         string   `yaml:"module,omitempty"`
	Service        string   `yaml:"service"`
	Endpoint       string   `yaml:"endpoint"`
	ContractDigest string   `yaml:"contract-digest"`
	Services       []string `yaml:"services,omitempty"`
	As             string   `yaml:"as,omitempty"`
}

// GenerateClientLibrary generates a codefly library — generated bindings plus
// a generated facade, per language — from one or more API contract entries,
// into req.Output. Every entry is generated into the same per-language
// directories, sharing one go.mod/package.json/pyproject.toml.
func GenerateClientLibrary(ctx context.Context, req *ClientLibraryRequest) error {
	if len(req.Entries) == 0 {
		return fmt.Errorf("at least one contract entry is required")
	}
	if len(req.Languages) == 0 {
		return fmt.Errorf("--language is required")
	}
	if err := checkContractDiamond(req.Entries); err != nil {
		return err
	}
	openAPICount := 0
	for i := range req.Entries {
		if req.Entries[i].Endpoint.Kind == composition.APIContractKindOpenAPI {
			openAPICount++
		}
	}
	if openAPICount > 0 && len(req.Entries) > 1 {
		return fmt.Errorf("cannot aggregate an OpenAPI contract with other entries in one library")
	}

	existingSources, err := readExistingLibrarySources(req.Output)
	if err != nil {
		return err
	}
	existingByKey := make(map[[3]string]librarySource, len(existingSources))
	for i := range existingSources {
		s := &existingSources[i]
		existingByKey[[3]string{s.Module, s.Service, s.Endpoint}] = *s
	}
	anyDigestChanged := false
	for i := range req.Entries {
		entry := &req.Entries[i]
		key := [3]string{entry.ModuleName, entry.Endpoint.Service, entry.Endpoint.Endpoint}
		prior, ok := existingByKey[key]
		if !ok || prior.ContractDigest == entry.Endpoint.Digest {
			continue
		}
		if !req.Force {
			return fmt.Errorf("library %s already has %s/%s (module %s) at a different contract (digest %s, generating %s); use --force to overwrite",
				req.Name, entry.Endpoint.Service, entry.Endpoint.Endpoint, entry.ModuleName, prior.ContractDigest, entry.Endpoint.Digest)
		}
		anyDigestChanged = true
	}

	// Only carry a previously-declared language forward when every entry this
	// run generated is the same contract that produced it (no digest changed):
	// a --force digest override means at least one contract changed, so a
	// language directory this run does not touch is now stale relative to it
	// and the manifest must stop vouching for it.
	var existingLanguages []generatedLanguageExport
	if !anyDigestChanged {
		existingLanguages, err = readExistingLanguageExports(req.Output)
		if err != nil {
			return err
		}
	}

	if err := writeContractFiles(ctx, req.Output, req.Entries); err != nil {
		return fmt.Errorf("cannot write contract: %w", err)
	}

	pyPackage := strings.ReplaceAll(req.Name, "-", "_")

	var langExports []generatedLanguageExport
	for _, lang := range req.Languages {
		if err := ctx.Err(); err != nil {
			return err
		}
		export, genErr := generateLanguageForEntries(ctx, lang, filepath.Join(req.Output, string(lang)), req.Entries, !req.NoFacade, req.GoModule, req.NpmPackage, pyPackage)
		if genErr != nil {
			return fmt.Errorf("cannot generate %s client: %w", lang, genErr)
		}
		langExports = append(langExports, export)
		cli.Info("generated %s client at %s", lang, filepath.Join(req.Output, string(lang)))
	}

	langExports = mergeLanguageExports(existingLanguages, langExports, req.Languages)

	if err := writeLibraryManifest(ctx, req.Output, req.Name, req.Entries, langExports, !req.NoFacade); err != nil {
		return fmt.Errorf("cannot write %s: %w", resources.LibraryConfigurationName, err)
	}

	cli.Header(1, "Library %s generated at %s", req.Name, req.Output)
	return nil
}

// checkContractDiamond groups entries by protobuf package and errors when a
// package is consumed at more than one contract digest — two producers (or
// two versions of one) claiming the same proto package cannot both be baked
// into one generated library, the same rule pkg/composition/coherence.go
// enforces for shared library majors.
func checkContractDiamond(entries []ContractEntry) error {
	type sighting struct{ digest, module, service, endpoint string }
	byPackage := map[string][]sighting{}
	for i := range entries {
		entry := &entries[i]
		if entry.Endpoint.Kind != composition.APIContractKindProtobuf {
			continue
		}
		byPackage[entry.Endpoint.Package] = append(byPackage[entry.Endpoint.Package], sighting{
			digest: entry.Endpoint.Digest, module: entry.ModuleName, service: entry.Endpoint.Service, endpoint: entry.Endpoint.Endpoint,
		})
	}
	packages := make([]string, 0, len(byPackage))
	for pkg := range byPackage {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		digests := map[string]bool{}
		for _, s := range byPackage[pkg] {
			digests[s.digest] = true
		}
		if len(digests) <= 1 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "contract %s is consumed at two versions:", pkg)
		for _, s := range byPackage[pkg] {
			fmt.Fprintf(&b, "\n  %s required by %s/%s/%s", s.digest, s.module, s.service, s.endpoint)
		}
		return fmt.Errorf("%s", b.String())
	}
	return nil
}

// generateLanguageForEntries generates one language's export for the library.
// An OpenAPI contract has no facade to aggregate and is only ever generated
// alone (GenerateClientLibrary refuses to mix it with other entries).
func generateLanguageForEntries(ctx context.Context, lang languages.Language, langDir string, entries []ContractEntry, facade bool, goModule, npmPackage, pyPackage string) (generatedLanguageExport, error) {
	if err := os.MkdirAll(langDir, 0o755); err != nil {
		return generatedLanguageExport{}, err
	}
	if len(entries) == 1 && entries[0].Endpoint.Kind == composition.APIContractKindOpenAPI {
		return generateOpenAPILanguageEntry(ctx, lang, langDir, &entries[0], goModule, npmPackage)
	}
	return generateProtobufLanguageEntries(ctx, lang, langDir, entries, facade, goModule, npmPackage, pyPackage)
}

// generateProtobufLanguageEntries wipes langDir's generated content once,
// then runs proto.GenerateClient once per entry and merges each result in —
// so N entries land in one gen/ tree with one facade file per entry, sharing
// one scaffold.
func generateProtobufLanguageEntries(ctx context.Context, lang languages.Language, langDir string, entries []ContractEntry, facade bool, goModule, npmPackage, pyPackage string) (generatedLanguageExport, error) {
	switch lang {
	case languages.GO:
		if err := wipeGoLibrary(langDir); err != nil {
			return generatedLanguageExport{}, err
		}
	case languages.TYPESCRIPT:
		if err := wipeTypeScriptLibrary(langDir); err != nil {
			return generatedLanguageExport{}, err
		}
	case languages.PYTHON:
		if err := wipePythonLibrary(filepath.Join(langDir, pyPackage)); err != nil {
			return generatedLanguageExport{}, err
		}
	default:
		return generatedLanguageExport{}, fmt.Errorf("language %s is not supported for client generation", lang)
	}

	var mapped []upstreamProtoModule
	for i := range entries {
		entry := &entries[i]
		marked, err := generateOneProtobufEntry(ctx, lang, langDir, entry, facade, goModule, pyPackage)
		if err != nil {
			return generatedLanguageExport{}, fmt.Errorf("entry %s/%s/%s: %w", entry.ModuleName, entry.Endpoint.Service, entry.Endpoint.Endpoint, err)
		}
		mapped = append(mapped, marked...)
	}

	switch lang {
	case languages.GO:
		return finalizeGoLibrary(langDir, goModule)
	case languages.TYPESCRIPT:
		return finalizeTypeScriptLibrary(langDir, npmPackage)
	case languages.PYTHON:
		return finalizePythonLibrary(langDir, pyPackage, pythonPackagesFor(mapped))
	default:
		return generatedLanguageExport{}, fmt.Errorf("language %s is not supported for client generation", lang)
	}
}

// generateOneProtobufEntry generates one contract entry into langDir and
// reports the upstream proto modules it dropped from the output, which the
// library's own dependency declaration must now cover.
func generateOneProtobufEntry(ctx context.Context, lang languages.Language, langDir string, entry *ContractEntry, facade bool, goModule, pyPackage string) ([]upstreamProtoModule, error) {
	raw, err := os.MkdirTemp("", "codefly-client-raw")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(raw) }()

	moduleName := entryModuleName(entry)
	req := coreproto.ClientRequest{
		Language:    lang,
		Destination: raw,
		Module:      moduleName,
		Services:    entry.Services,
		Facade:      facade,
	}
	var mapped []upstreamProtoModule
	if len(entry.ProtoSources) > 0 {
		req.Sources = entry.ProtoSources
	} else {
		var set descriptorpb.FileDescriptorSet
		if err := googleproto.Unmarshal(entry.ContractBytes, &set); err != nil {
			return nil, fmt.Errorf("cannot parse contract descriptor set: %w", err)
		}
		image, marked, err := markUpstreamModulesAsImports(&set, lang)
		if err != nil {
			return nil, fmt.Errorf("cannot prepare contract descriptor set: %w", err)
		}
		req.DescriptorSet = image
		req.TargetFiles = targetFilesForPackage(&set, entry.Endpoint.Package)
		mapped = marked
	}
	if err := coreproto.GenerateClient(ctx, req); err != nil {
		return nil, err
	}

	switch lang {
	case languages.GO:
		return mapped, mergeGoEntry(langDir, raw, moduleName, goModule)
	case languages.TYPESCRIPT:
		return mapped, mergeTypeScriptEntry(langDir, raw)
	case languages.PYTHON:
		return mapped, mergePythonEntry(filepath.Join(langDir, pyPackage), raw, moduleName, pyPackage)
	default:
		return nil, fmt.Errorf("language %s is not supported for client generation", lang)
	}
}

// entryModuleName is the facade entry-point name proto.GenerateClient uses
// for entry: its own As override, else the facade plugin's own default, else
// the generic fallback.
func entryModuleName(entry *ContractEntry) string {
	if entry.As != "" {
		return entry.As
	}
	if entry.FacadeModuleDefault != "" {
		return entry.FacadeModuleDefault
	}
	return defaultFacadeModule
}

// generateOpenAPILanguageEntry synthesizes a basev0.Endpoint carrying the raw
// OpenAPI document (the only thing proto.GenerateOpenAPI reads) and runs it.
// OpenAPI clients have no facade: proto.GenerateOpenAPI's contract-derived
// codegen is a plain typed HTTP client.
func generateOpenAPILanguageEntry(ctx context.Context, lang languages.Language, langDir string, entry *ContractEntry, goModule, npmPackage string) (generatedLanguageExport, error) {
	return assembleOpenAPILibrary(ctx, lang, langDir, entry, goModule, npmPackage)
}

// upstreamProtoModule is a shared proto module a persisted contract carries
// among its imports and that already ships canonical bindings of its own.
//
// A module's files are identified by both their path prefix and their proto
// package, never the path alone: a module that keeps its own .proto under one
// of these directories (a vendored copy is the common reason) still declares
// its own package, and marking such a file as an import would silently drop
// the bindings this run exists to produce. A sub-package of the module's own
// counts as the module's, which covers both protovalidate's buf.validate.priv
// and google/protobuf/compiler/plugin.proto (package google.protobuf.compiler,
// a well-known type buf itself resolves from the protobuf runtime).
type upstreamProtoModule struct {
	pathPrefix   string
	protoPackage string

	// bufModule, when non-zero, names the buf.build module publishing these
	// files. The Go template's managed-mode go_package_prefix.except list names
	// the same module, so stamping the identity onto the image keeps their
	// go_package pointing at the canonical Go package instead of being
	// rewritten to the generated library's own path.
	bufModule bufModuleName

	// pythonPackage is the PyPI distribution shipping these bindings, which the
	// generated library must depend on once it stops carrying its own copy.
	// Empty for the well-known types: protobuf itself ships them, and the
	// scaffold already depends on protobuf.
	pythonPackage string
}

// bufModuleName is buf.alpha.module.v1.ModuleName — the module identity buf
// reads off an image file to resolve managed mode's except list.
type bufModuleName struct {
	remote     string
	owner      string
	repository string
}

func (n bufModuleName) isZero() bool { return n.repository == "" }

// identity renders the name the way buf's managed-mode except list spells it.
func (n bufModuleName) identity() string {
	return n.remote + "/" + n.owner + "/" + n.repository
}

const (
	googleapisPyPI    = "googleapis-common-protos"
	protovalidatePyPI = "protovalidate"
)

var (
	googleapisModule    = bufModuleName{remote: "buf.build", owner: "googleapis", repository: "googleapis"}
	protovalidateModule = bufModuleName{remote: "buf.build", owner: "bufbuild", repository: "protovalidate"}
)

// upstreamProtoModules are the shared proto modules buf must never vendor
// into a generated library: a local copy registers a proto file the consumer's
// upstream module registers too, and both Go's and Python's registries abort
// on the duplicate. Vendoring is only ever correct for the module's own
// first-party protos.
//
// Go and Python resolve a dropped file to its upstream package because their
// generated bindings reference it by an absolute path — a rewritten go_package
// for Go, an untouched `from buf.validate import ...` for Python (only the
// roots actually present in the generated tree get rewritten under the
// library's own package). TypeScript cannot: protobuf-es emits a relative
// import of the dependency's own generated file, which no npm package can
// satisfy, so TypeScript keeps its local copies.
var upstreamProtoModules = []upstreamProtoModule{
	{pathPrefix: "google/protobuf/", protoPackage: "google.protobuf"},
	{pathPrefix: "google/api/", protoPackage: "google.api", bufModule: googleapisModule, pythonPackage: googleapisPyPI},
	{pathPrefix: "google/rpc/", protoPackage: "google.rpc", bufModule: googleapisModule, pythonPackage: googleapisPyPI},
	{pathPrefix: "google/type/", protoPackage: "google.type", bufModule: googleapisModule, pythonPackage: googleapisPyPI},
	{pathPrefix: "buf/validate/", protoPackage: "buf.validate", bufModule: protovalidateModule, pythonPackage: protovalidatePyPI},
}

// buf marks a file that an image carries only to resolve imports with
// buf.alpha.image.v1.ImageFileExtension.is_import, and records which buf
// module it came from in module_info: an extension of FileDescriptorProto at
// field 8042, carrying is_import at field 1 and module_info at field 2, whose
// own name field 1 is a ModuleName of remote, owner and repository.
const (
	bufImageFileExtensionField   = 8042
	bufImageFileIsImportField    = 1
	bufImageFileModuleInfoField  = 2
	bufModuleInfoNameField       = 1
	bufModuleNameRemoteField     = 1
	bufModuleNameOwnerField      = 2
	bufModuleNameRepositoryField = 3
)

// markUpstreamModulesAsImports returns set re-serialized as a buf image whose
// upstream-module files are marked as imports — each stamped with its buf
// module identity when it has one — along with the distinct modules it marked.
// set itself is left untouched: callers keep reading the parsed contract after
// this returns.
//
// A persisted contract is a plain FileDescriptorSet — `generate contracts`
// builds it with `buf build --as-file-descriptor-set`, which deliberately drops
// buf's image extensions — so buf treats every file it carries as a generation
// target and emits local bindings for the imports too. That is wrong twice
// over. For the well-known types it puts one Go package per type (descriptorpb,
// durationpb, ...) in a single gen/google/protobuf directory, which the
// compiler rejects outright. For googleapis and protovalidate it compiles, but
// registers files the upstream modules also register: a consumer that links
// both — anything using buf.build/go/protovalidate, say — panics in init on a
// duplicate registration of buf/validate/validate.proto.
func markUpstreamModulesAsImports(set *descriptorpb.FileDescriptorSet, lang languages.Language) ([]byte, []upstreamProtoModule, error) {
	image, ok := googleproto.Clone(set).(*descriptorpb.FileDescriptorSet)
	if !ok {
		return nil, nil, fmt.Errorf("cannot copy contract descriptor set")
	}
	var marked []upstreamProtoModule
	seen := map[string]bool{}
	for _, file := range image.GetFile() {
		module, found := upstreamModuleFor(file, lang)
		if !found {
			continue
		}
		message := file.ProtoReflect()
		unknown := append(protoreflect.RawFields(nil), message.GetUnknown()...)
		message.SetUnknown(append(unknown, bufImageFileExtension(&module)...))
		if key := module.bufModule.identity(); !seen[key] {
			seen[key] = true
			marked = append(marked, module)
		}
	}
	data, err := googleproto.Marshal(image)
	if err != nil {
		return nil, nil, err
	}
	return data, marked, nil
}

func upstreamModuleFor(file *descriptorpb.FileDescriptorProto, lang languages.Language) (upstreamProtoModule, bool) {
	for _, module := range upstreamProtoModules {
		if !module.bufModule.isZero() && lang == languages.TYPESCRIPT {
			continue
		}
		if !strings.HasPrefix(file.GetName(), module.pathPrefix) {
			continue
		}
		pkg := file.GetPackage()
		if pkg == module.protoPackage || strings.HasPrefix(pkg, module.protoPackage+".") {
			return module, true
		}
	}
	return upstreamProtoModule{}, false
}

func bufImageFileExtension(module *upstreamProtoModule) []byte {
	extension := protowire.AppendVarint(protowire.AppendTag(nil, bufImageFileIsImportField, protowire.VarintType), 1)
	if !module.bufModule.isZero() {
		name := protowire.AppendString(protowire.AppendTag(nil, bufModuleNameRemoteField, protowire.BytesType), module.bufModule.remote)
		name = protowire.AppendString(protowire.AppendTag(name, bufModuleNameOwnerField, protowire.BytesType), module.bufModule.owner)
		name = protowire.AppendString(protowire.AppendTag(name, bufModuleNameRepositoryField, protowire.BytesType), module.bufModule.repository)
		info := protowire.AppendBytes(protowire.AppendTag(nil, bufModuleInfoNameField, protowire.BytesType), name)
		extension = protowire.AppendBytes(protowire.AppendTag(extension, bufImageFileModuleInfoField, protowire.BytesType), info)
	}
	return protowire.AppendBytes(protowire.AppendTag(nil, bufImageFileExtensionField, protowire.BytesType), extension)
}

// pythonPackagesFor is the set of PyPI distributions the modules dropped from
// a Python library must now be resolved from.
func pythonPackagesFor(modules []upstreamProtoModule) []string {
	var packages []string
	seen := map[string]bool{}
	for _, module := range modules {
		if module.pythonPackage == "" || seen[module.pythonPackage] {
			continue
		}
		seen[module.pythonPackage] = true
		packages = append(packages, module.pythonPackage)
	}
	sort.Strings(packages)
	return packages
}

// targetFilesForPackage returns the names of the files in set whose proto
// package is pkg — the module's own files, as opposed to shared imports —
// which proto.GenerateClient's TargetFiles needs for a Python facade.
func targetFilesForPackage(set *descriptorpb.FileDescriptorSet, pkg string) []string {
	var files []string
	for _, f := range set.GetFile() {
		if f.GetPackage() == pkg {
			files = append(files, f.GetName())
		}
	}
	sort.Strings(files)
	return files
}

// readExistingLibrarySources reads the sources: entries an existing
// libraries/<name>/library.codefly.yaml already declares, if any.
func readExistingLibrarySources(outputDir string) ([]librarySource, error) {
	data, err := os.ReadFile(filepath.Join(outputDir, resources.LibraryConfigurationName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Sources []librarySource `yaml:"sources"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse existing %s: %w", resources.LibraryConfigurationName, err)
	}
	return doc.Sources, nil
}

// readExistingLanguageExports reads the languages: entries an existing
// libraries/<name>/library.codefly.yaml already declares, if any.
func readExistingLanguageExports(outputDir string) ([]generatedLanguageExport, error) {
	data, err := os.ReadFile(filepath.Join(outputDir, resources.LibraryConfigurationName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Languages []generatedLanguageExport `yaml:"languages"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse existing %s: %w", resources.LibraryConfigurationName, err)
	}
	return doc.Languages, nil
}

// existingTopVersion reads an existing manifest's top-level version:, or
// "0.1.0" when there is none to read — the seed for a brand new aggregated
// library, which (unlike a single-entry generate client library) has no one
// producer version to inherit.
func existingTopVersion(outputDir string) string {
	data, err := os.ReadFile(filepath.Join(outputDir, resources.LibraryConfigurationName))
	if err != nil {
		return "0.1.0"
	}
	var doc struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Version == "" {
		return "0.1.0"
	}
	return doc.Version
}

// mergeLanguageExports combines this run's freshly generated exports with an
// existing manifest's, so requesting a subset of languages (e.g. --language
// go to refresh just one binding) does not silently drop the declaration of
// languages generated by an earlier run and left untouched by this one.
func mergeLanguageExports(existing, fresh []generatedLanguageExport, requested []languages.Language) []generatedLanguageExport {
	requestedNames := make(map[string]bool, len(requested))
	for _, l := range requested {
		requestedNames[string(l)] = true
	}
	merged := make([]generatedLanguageExport, 0, len(existing)+len(fresh))
	seen := make(map[string]bool, len(existing)+len(fresh))
	for _, e := range fresh {
		merged = append(merged, e)
		seen[e.Name] = true
	}
	for _, e := range existing {
		if requestedNames[e.Name] || seen[e.Name] {
			continue
		}
		merged = append(merged, e)
		seen[e.Name] = true
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	return merged
}

// writeContractFiles writes a generated library's contract/ directory: one
// flat contract.binpb|openapi.json + catalog.codefly.json for a single entry
// (the `generate client` shape), or one such pair per entry under its own
// subdirectory when aggregating more than one (sync solution-sdk).
func writeContractFiles(ctx context.Context, outputDir string, entries []ContractEntry) error {
	contractRoot := filepath.Join(outputDir, "contract")
	if err := os.RemoveAll(contractRoot); err != nil {
		return err
	}
	if len(entries) == 1 {
		return writeOneContractDir(ctx, contractRoot, &entries[0])
	}
	for i := range entries {
		entry := &entries[i]
		dir := filepath.Join(contractRoot, contractEntryDirName(entry))
		if err := writeOneContractDir(ctx, dir, entry); err != nil {
			return err
		}
	}
	return nil
}

func contractEntryDirName(entry *ContractEntry) string {
	module := entry.ModuleName
	if module == "" {
		module = localPackageID
	}
	return fmt.Sprintf("%s-%s-%s", module, entry.Endpoint.Service, entry.Endpoint.Endpoint)
}

func writeOneContractDir(ctx context.Context, contractDir string, entry *ContractEntry) error {
	if err := os.MkdirAll(contractDir, 0o755); err != nil {
		return err
	}
	filename := "contract.binpb"
	if entry.Endpoint.Kind == composition.APIContractKindOpenAPI {
		filename = "openapi.json"
	}
	if err := shared.WriteFileAtomic(ctx, filepath.Join(contractDir, filename), entry.ContractBytes, 0o644); err != nil {
		return err
	}

	catalogEntry := entry.Endpoint
	catalogEntry.Path = filename
	packageID := entry.PackageID
	if packageID == "" {
		packageID = localPackageID
	}
	catalog := &composition.APIContractCatalog{
		Schema:    composition.APIContractCatalogSchema,
		Package:   packageID,
		Version:   entry.PackageVersion,
		Endpoints: []composition.APIContractEndpoint{catalogEntry},
	}
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("generated catalog is invalid: %w", err)
	}
	data, err := catalog.CanonicalBytes()
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, filepath.Join(contractDir, "catalog.codefly.json"), data, 0o644)
}

// writeLibraryManifest writes library.codefly.yaml. It is written directly
// (not through resources.Library, whose fields are a subset of this schema)
// so the sources: and generated-by: provenance survives; resources.Library
// still loads the file fine, since a plain YAML decode ignores fields it does
// not declare.
func writeLibraryManifest(ctx context.Context, outputDir, name string, entries []ContractEntry, langExports []generatedLanguageExport, facade bool) error {
	type languageEntry struct {
		Name    string   `yaml:"name"`
		Path    string   `yaml:"path"`
		Exports []string `yaml:"exports"`
	}
	type generatedByEntry struct {
		Codefly   string `yaml:"codefly"`
		Companion string `yaml:"companion,omitempty"`
		Facade    bool   `yaml:"facade"`
	}
	type manifest struct {
		Kind        string           `yaml:"kind"`
		Name        string           `yaml:"name"`
		Version     string           `yaml:"version"`
		Languages   []languageEntry  `yaml:"languages"`
		Sources     []librarySource  `yaml:"sources"`
		GeneratedBy generatedByEntry `yaml:"generated-by"`
	}

	languageEntries := make([]languageEntry, 0, len(langExports))
	for _, export := range langExports {
		languageEntries = append(languageEntries, languageEntry(export))
	}

	sources := make([]librarySource, 0, len(entries))
	for i := range entries {
		entry := &entries[i]
		packageID := entry.PackageID
		if packageID == "" {
			packageID = localPackageID
		}
		sources = append(sources, librarySource{
			Package:        packageID,
			Version:        entry.PackageVersion,
			Module:         entry.ModuleName,
			Service:        entry.Endpoint.Service,
			Endpoint:       entry.Endpoint.Endpoint,
			ContractDigest: entry.Endpoint.Digest,
			Services:       protobufServiceNames(entry.Endpoint.Services),
			As:             entry.As,
		})
	}

	version := entries[0].PackageVersion
	if len(entries) > 1 {
		version = existingTopVersion(outputDir)
	}

	codeflyVersion, _ := cli.GetCurrentVersion()
	companion := ""
	if image, err := coreproto.CompanionImage(ctx); err == nil {
		companion = image.FullName()
	}

	doc := manifest{
		Kind:      "library",
		Name:      name,
		Version:   version,
		Languages: languageEntries,
		Sources:   sources,
		GeneratedBy: generatedByEntry{
			Codefly:   codeflyVersion,
			Companion: companion,
			Facade:    facade && anyProtobuf(entries),
		},
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, filepath.Join(outputDir, resources.LibraryConfigurationName), data, 0o644)
}

func anyProtobuf(entries []ContractEntry) bool {
	for i := range entries {
		if entries[i].Endpoint.Kind == composition.APIContractKindProtobuf {
			return true
		}
	}
	return false
}

// ReadCatalogContractBytes reads an entry's contract file off disk and
// verifies it still matches the catalog's recorded digest, so a corrupted or
// stale contract file never silently gets baked into a generated library.
func ReadCatalogContractBytes(moduleDir string, entry *composition.APIContractEndpoint) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(entry.Path)))
	if err != nil {
		return nil, fmt.Errorf("cannot read contract file %s: %w", entry.Path, err)
	}
	if digest := composition.APIContractDigest(data); digest != entry.Digest {
		return nil, fmt.Errorf("contract file %s digest %s does not match catalog digest %s", entry.Path, digest, entry.Digest)
	}
	return data, nil
}

// DefaultFacadeModuleName replicates the facade plugins' own default (the
// last non-version segment of the proto package), so the CLI knows the exact
// facade file name it must locate after generation without forcing the user
// to pass a module-name override. Empty for an OpenAPI contract, which has no
// facade.
func DefaultFacadeModuleName(entry *composition.APIContractEndpoint) string {
	if entry.Kind != composition.APIContractKindProtobuf {
		return ""
	}
	segments := strings.Split(entry.Package, ".")
	for len(segments) > 1 && isProtoVersionSegment(segments[len(segments)-1]) {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

func isProtoVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func protobufServiceNames(services []composition.APIContractService) []string {
	if len(services) == 0 {
		return nil
	}
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Name
	}
	return names
}
