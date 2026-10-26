package generators

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/core/composition"
	"google.golang.org/protobuf/encoding/protowire"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// narrowEntries narrows a library to the services its entries select.
//
// A protobuf entry naming Services binds those services and nothing else, so
// its contract is rewritten to declare only them before anything is generated:
// the persisted descriptor set, the catalog and the manifest describe the
// narrowed contract, and the bindings and facade are generated from it. The
// narrowed set keeps, from the files declaring the selected services and the
// files those reach, the top-level declarations the services need: their
// methods' messages and, transitively, every message and enum a kept message
// names (a nested type keeps the top-level message holding it). A file
// defining a custom option a kept declaration sets is kept whole, with its
// imports, and so is a file of a shared upstream module: neither is the
// library's own, and the upstream ones keep the shape their distributions
// publish. No file declares a service that is not selected.
//
// The narrowing is the library's, not one entry's: an aggregated library
// generates every entry into one tree, so a file two entries carry must come
// out the same for both. The kept declarations are computed across every
// entry at once, and an entry naming no Services keeps its whole contract
// (and is returned untouched). A library in which no entry names Services is
// returned as is.
func narrowEntries(entries []ContractEntry) ([]ContractEntry, error) {
	if !slices.ContainsFunc(entries, func(entry ContractEntry) bool { return selectsServices(&entry) }) {
		return entries, nil
	}
	n := &narrowing{
		files:      map[string]*descriptorpb.FileDescriptorProto{},
		upstream:   map[string]bool{},
		types:      map[string]declaration{},
		services:   map[string]string{},
		extensions: map[extensionKey]string{},
		selected:   map[string]bool{},
		kept:       map[string]*keptFile{},
	}
	sets := make([]*descriptorpb.FileDescriptorSet, len(entries))
	for i := range entries {
		if entries[i].Endpoint.Kind != composition.APIContractKindProtobuf {
			continue
		}
		set := &descriptorpb.FileDescriptorSet{}
		// No resolver: custom options stay unknown fields, so the narrowed
		// bytes depend on the contract alone, never on what this binary links.
		if err := (googleproto.UnmarshalOptions{Resolver: new(protoregistry.Types)}).Unmarshal(entries[i].ContractBytes, set); err != nil {
			return nil, fmt.Errorf("entry %s: cannot parse contract descriptor set: %w", entryLabel(&entries[i]), err)
		}
		sets[i] = set
		n.index(set)
	}

	selections := make([][]composition.APIContractService, len(entries))
	for i := range entries {
		if sets[i] == nil {
			continue
		}
		if !selectsServices(&entries[i]) {
			for _, file := range sets[i].GetFile() {
				n.keepWhole(file.GetName())
				for _, service := range file.GetService() {
					n.selected[qualify(file.GetPackage(), service.GetName())] = true
				}
			}
			continue
		}
		selected, err := selectedServices(&entries[i])
		if err != nil {
			return nil, err
		}
		selections[i] = selected
		for _, service := range selected {
			n.selected[service.FullName] = true
		}
	}
	roots := make([]string, 0, len(n.selected))
	for fullName := range n.selected {
		roots = append(roots, fullName)
	}
	sort.Strings(roots)
	for _, fullName := range roots {
		n.keepService(fullName)
	}
	if n.err != nil {
		return nil, fmt.Errorf("cannot narrow the contract to its selected services: %w", n.err)
	}

	narrowed := slices.Clone(entries)
	for i := range narrowed {
		if selections[i] == nil {
			continue
		}
		entry := &narrowed[i]
		data, err := n.emit(sets[i])
		if err != nil {
			return nil, fmt.Errorf("entry %s: %w", entryLabel(entry), err)
		}
		entry.ContractBytes = data
		entry.Endpoint.Services = selections[i]
		entry.Endpoint.Digest = composition.APIContractDigest(data)
		// The proto sources declare the whole contract; the narrowed
		// descriptor set is what is generated from.
		entry.ProtoSources = nil
	}
	return narrowed, nil
}

func selectsServices(entry *ContractEntry) bool {
	return entry.Endpoint.Kind == composition.APIContractKindProtobuf && len(entry.Services) > 0
}

func entryLabel(entry *ContractEntry) string {
	return entry.ModuleName + "/" + entry.Endpoint.Service + "/" + entry.Endpoint.Endpoint
}

// selectedServices is the catalog entries of the services entry names, in
// catalog order. A name the contract does not declare is refused by name.
func selectedServices(entry *ContractEntry) ([]composition.APIContractService, error) {
	declared := map[string]bool{}
	for _, service := range entry.Endpoint.Services {
		declared[service.Name] = true
	}
	wanted := map[string]bool{}
	var unknown []string
	for _, name := range entry.Services {
		name = strings.TrimSpace(name)
		if !declared[name] {
			unknown = append(unknown, fmt.Sprintf("%q", name))
		}
		wanted[name] = true
	}
	if len(unknown) > 0 {
		names := make([]string, 0, len(declared))
		for name := range declared {
			names = append(names, name)
		}
		sort.Strings(names)
		sort.Strings(unknown)
		return nil, fmt.Errorf("entry %s: services %s are not declared by contract %s, which declares %s",
			entryLabel(entry), strings.Join(unknown, ", "), entry.Endpoint.Package, strings.Join(names, ", "))
	}
	var selected []composition.APIContractService
	for _, service := range entry.Endpoint.Services {
		if !wanted[service.Name] {
			continue
		}
		if service.FullName == "" {
			service.FullName = qualify(entry.Endpoint.Package, service.Name)
		}
		selected = append(selected, service)
	}
	return selected, nil
}

// narrowing is the closure of what a library's selected services need, over
// every file its entries' contracts carry (by name, first seen).
type narrowing struct {
	files      map[string]*descriptorpb.FileDescriptorProto
	upstream   map[string]bool
	types      map[string]declaration // message or enum full name -> its top-level declaration
	services   map[string]string      // service full name -> file
	extensions map[extensionKey]string
	selected   map[string]bool // service full names
	kept       map[string]*keptFile
	err        error
}

// declaration is a top-level message or enum of a file.
type declaration struct {
	file    string
	message *descriptorpb.DescriptorProto
	enum    *descriptorpb.EnumDescriptorProto
}

type extensionKey struct {
	extendee string
	number   int32
}

// keptFile is what a file keeps: everything when whole, otherwise the named
// top-level declarations, and the imports they need.
type keptFile struct {
	whole    bool
	messages map[string]bool
	enums    map[string]bool
	imports  map[string]bool
}

func qualify(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

func (n *narrowing) fail(format string, args ...any) {
	if n.err == nil {
		n.err = fmt.Errorf(format, args...)
	}
}

func (n *narrowing) index(set *descriptorpb.FileDescriptorSet) {
	for _, file := range set.GetFile() {
		name := file.GetName()
		if _, seen := n.files[name]; seen {
			continue
		}
		n.files[name] = file
		if _, upstream := matchUpstreamModule(file); upstream {
			n.upstream[name] = true
		}
		pkg := file.GetPackage()
		for _, message := range file.GetMessageType() {
			n.indexMessage(qualify(pkg, message.GetName()), declaration{file: name, message: message}, message)
		}
		for _, enum := range file.GetEnumType() {
			n.types[qualify(pkg, enum.GetName())] = declaration{file: name, enum: enum}
		}
		for _, service := range file.GetService() {
			n.services[qualify(pkg, service.GetName())] = name
		}
		n.indexExtensions(name, file.GetExtension())
	}
}

func (n *narrowing) indexMessage(fullName string, top declaration, message *descriptorpb.DescriptorProto) {
	n.types[fullName] = top
	for _, nested := range message.GetNestedType() {
		n.indexMessage(fullName+"."+nested.GetName(), top, nested)
	}
	for _, enum := range message.GetEnumType() {
		n.types[fullName+"."+enum.GetName()] = top
	}
	n.indexExtensions(top.file, message.GetExtension())
}

func (n *narrowing) indexExtensions(file string, extensions []*descriptorpb.FieldDescriptorProto) {
	for _, extension := range extensions {
		n.extensions[extensionKey{strings.TrimPrefix(extension.GetExtendee(), "."), extension.GetNumber()}] = file
	}
}

// file is name's kept state, created (and its file options visited) the first
// time anything of it is kept.
func (n *narrowing) file(name string) *keptFile {
	if kept, ok := n.kept[name]; ok {
		return kept
	}
	kept := &keptFile{messages: map[string]bool{}, enums: map[string]bool{}, imports: map[string]bool{}}
	n.kept[name] = kept
	if file := n.files[name]; file != nil {
		n.visitOptions(name, file.GetOptions())
	} else {
		n.fail("the contract imports %s but does not carry it", name)
	}
	return kept
}

func (n *narrowing) keepWhole(name string) {
	kept := n.file(name)
	if kept.whole {
		return
	}
	kept.whole = true
	for _, dependency := range n.files[name].GetDependency() {
		n.keepWhole(dependency)
	}
}

func (n *narrowing) keepService(fullName string) {
	name, ok := n.services[fullName]
	if !ok {
		n.fail("the contract declares no service %s", fullName)
		return
	}
	n.file(name)
	file := n.files[name]
	for _, service := range file.GetService() {
		if qualify(file.GetPackage(), service.GetName()) != fullName {
			continue
		}
		n.visitOptions(name, service.GetOptions())
		for _, method := range service.GetMethod() {
			n.visitOptions(name, method.GetOptions())
			n.useType(name, method.GetInputType())
			n.useType(name, method.GetOutputType())
		}
	}
}

// useType keeps the top-level declaration typeName resolves to, and the
// import from reaches it through.
func (n *narrowing) useType(from, typeName string) {
	fullName := strings.TrimPrefix(typeName, ".")
	decl, ok := n.types[fullName]
	if !ok {
		n.fail("%s names %s, which no file of the contract declares", from, fullName)
		return
	}
	n.require(from, decl.file)
	kept := n.file(decl.file)
	switch {
	case kept.whole:
	case n.upstream[decl.file]:
		n.keepWhole(decl.file)
	case decl.message != nil && !kept.messages[decl.message.GetName()]:
		kept.messages[decl.message.GetName()] = true
		n.visitMessage(decl.file, decl.message)
	case decl.enum != nil && !kept.enums[decl.enum.GetName()]:
		kept.enums[decl.enum.GetName()] = true
		n.visitEnum(decl.file, decl.enum)
	}
}

func (n *narrowing) visitMessage(file string, message *descriptorpb.DescriptorProto) {
	n.visitOptions(file, message.GetOptions())
	for _, field := range message.GetField() {
		n.visitField(file, field)
	}
	for _, extension := range message.GetExtension() {
		n.visitField(file, extension)
		n.useType(file, extension.GetExtendee())
	}
	for _, oneof := range message.GetOneofDecl() {
		n.visitOptions(file, oneof.GetOptions())
	}
	for _, nested := range message.GetNestedType() {
		n.visitMessage(file, nested)
	}
	for _, enum := range message.GetEnumType() {
		n.visitEnum(file, enum)
	}
}

func (n *narrowing) visitField(file string, field *descriptorpb.FieldDescriptorProto) {
	n.visitOptions(file, field.GetOptions())
	if field.GetTypeName() != "" {
		n.useType(file, field.GetTypeName())
	}
}

func (n *narrowing) visitEnum(file string, enum *descriptorpb.EnumDescriptorProto) {
	n.visitOptions(file, enum.GetOptions())
	for _, value := range enum.GetValue() {
		n.visitOptions(file, value.GetOptions())
	}
}

// visitOptions keeps, whole, every file defining a custom option set in
// options: a field the options message does not declare is an extension.
func (n *narrowing) visitOptions(from string, options googleproto.Message) {
	if options == nil || !options.ProtoReflect().IsValid() {
		return
	}
	descriptor := options.ProtoReflect().Descriptor()
	data, err := googleproto.MarshalOptions{Deterministic: true}.Marshal(options)
	if err != nil {
		n.fail("%s: cannot read %s: %w", from, descriptor.FullName(), err)
		return
	}
	for len(data) > 0 {
		number, kind, length := protowire.ConsumeTag(data)
		if length < 0 {
			n.fail("%s: malformed %s", from, descriptor.FullName())
			return
		}
		data = data[length:]
		if length = protowire.ConsumeFieldValue(number, kind, data); length < 0 {
			n.fail("%s: malformed %s", from, descriptor.FullName())
			return
		}
		data = data[length:]
		if descriptor.Fields().ByNumber(number) != nil {
			continue
		}
		defining, ok := n.extensions[extensionKey{string(descriptor.FullName()), int32(number)}]
		if !ok {
			n.fail("%s sets option %d of %s, which no file of the contract defines", from, number, descriptor.FullName())
			continue
		}
		n.require(from, defining)
		n.keepWhole(defining)
	}
}

// require records that from needs target: the import of from that reaches it,
// directly or through a chain of public imports, each of which is kept.
func (n *narrowing) require(from, target string) {
	if from == target {
		return
	}
	chain := n.importChain(from, target)
	if chain == nil {
		n.fail("%s needs %s but does not import it", from, target)
		return
	}
	for _, hop := range chain {
		n.file(from).imports[hop] = true
		from = hop
	}
}

func (n *narrowing) importChain(from, target string) []string {
	dependencies := n.files[from].GetDependency()
	if slices.Contains(dependencies, target) {
		return []string{target}
	}
	for _, dependency := range dependencies {
		if chain := n.publicChain(dependency, target, map[string]bool{}); chain != nil {
			return chain
		}
	}
	return nil
}

func (n *narrowing) publicChain(file, target string, seen map[string]bool) []string {
	if file == target {
		return []string{file}
	}
	if seen[file] || n.files[file] == nil {
		return nil
	}
	seen[file] = true
	descriptor := n.files[file]
	for _, index := range descriptor.GetPublicDependency() {
		if chain := n.publicChain(descriptor.GetDependency()[index], target, seen); chain != nil {
			return append([]string{file}, chain...)
		}
	}
	return nil
}

// emit is set narrowed to what was kept, in set's file order, refused unless
// it resolves on its own.
func (n *narrowing) emit(set *descriptorpb.FileDescriptorSet) ([]byte, error) {
	narrowed := &descriptorpb.FileDescriptorSet{}
	for _, file := range set.GetFile() {
		if kept, ok := n.kept[file.GetName()]; ok {
			narrowed.File = append(narrowed.File, n.narrowFile(file, kept))
		}
	}
	if _, err := protodesc.NewFiles(narrowed); err != nil {
		return nil, fmt.Errorf("the narrowed contract does not resolve: %w", err)
	}
	return googleproto.MarshalOptions{Deterministic: true}.Marshal(narrowed)
}

func (n *narrowing) narrowFile(file *descriptorpb.FileDescriptorProto, kept *keptFile) *descriptorpb.FileDescriptorProto {
	out, _ := googleproto.Clone(file).(*descriptorpb.FileDescriptorProto)
	messages := keptIndexes(len(file.GetMessageType()), func(i int) bool {
		return kept.whole || kept.messages[file.GetMessageType()[i].GetName()]
	})
	enums := keptIndexes(len(file.GetEnumType()), func(i int) bool {
		return kept.whole || kept.enums[file.GetEnumType()[i].GetName()]
	})
	services := keptIndexes(len(file.GetService()), func(i int) bool {
		return n.selected[qualify(file.GetPackage(), file.GetService()[i].GetName())]
	})
	extensions := keptIndexes(len(file.GetExtension()), func(int) bool { return kept.whole })
	dependencies := keptIndexes(len(file.GetDependency()), func(i int) bool {
		return kept.whole || kept.imports[file.GetDependency()[i]]
	})
	public := keptIndexes(len(file.GetPublicDependency()), func(i int) bool { return dependencies[file.GetPublicDependency()[i]] >= 0 })
	weak := keptIndexes(len(file.GetWeakDependency()), func(i int) bool { return dependencies[file.GetWeakDependency()[i]] >= 0 })

	out.MessageType = pick(out.GetMessageType(), messages)
	out.EnumType = pick(out.GetEnumType(), enums)
	out.Service = pick(out.GetService(), services)
	out.Extension = pick(out.GetExtension(), extensions)
	out.Dependency = pick(out.GetDependency(), dependencies)
	out.PublicDependency = reindex(pick(out.GetPublicDependency(), public), dependencies)
	out.WeakDependency = reindex(pick(out.GetWeakDependency(), weak), dependencies)

	// Source info locates a comment by index into these lists, so a comment
	// follows its declaration and a dropped declaration's comments go with it.
	if info := out.GetSourceCodeInfo(); info != nil {
		moved := map[int32][]int32{
			fileField("message_type"):      messages,
			fileField("enum_type"):         enums,
			fileField("service"):           services,
			fileField("extension"):         extensions,
			fileField("dependency"):        dependencies,
			fileField("public_dependency"): public,
			fileField("weak_dependency"):   weak,
		}
		locations := info.GetLocation()[:0]
		for _, location := range info.GetLocation() {
			path := location.GetPath()
			if mapping, ok := moved[firstOrNegative(path)]; ok && len(path) > 1 {
				if int(path[1]) >= len(mapping) || mapping[path[1]] < 0 {
					continue
				}
				path[1] = mapping[path[1]]
			}
			locations = append(locations, location)
		}
		info.Location = locations
	}
	return out
}

func firstOrNegative(path []int32) int32 {
	if len(path) == 0 {
		return -1
	}
	return path[0]
}

// fileField is FileDescriptorProto's field number for name, read from its
// descriptor rather than retyped.
func fileField(name protoreflect.Name) int32 {
	return int32((&descriptorpb.FileDescriptorProto{}).ProtoReflect().Descriptor().Fields().ByName(name).Number())
}

// keptIndexes maps each of count indexes to its index once the ones keep
// refuses are dropped, or -1 for a dropped one.
func keptIndexes(count int, keep func(int) bool) []int32 {
	mapping := make([]int32, count)
	var next int32
	for i := range mapping {
		if keep(i) {
			mapping[i] = next
			next++
		} else {
			mapping[i] = -1
		}
	}
	return mapping
}

func pick[T any](items []T, mapping []int32) []T {
	var kept []T
	for i, item := range items {
		if mapping[i] >= 0 {
			kept = append(kept, item)
		}
	}
	return kept
}

func reindex(indexes []int32, mapping []int32) []int32 {
	for i, index := range indexes {
		indexes[i] = mapping[index]
	}
	return indexes
}
