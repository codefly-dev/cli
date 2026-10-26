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
// A protobuf entry naming Services binds those services and nothing else. Its
// persisted contract (contract.binpb and its catalog) declares only them: the
// files declaring them and, of the files those reach, the top-level
// declarations they need — their methods' messages and, transitively, every
// message and enum a kept message names (a nested type keeps the top-level
// message holding it), and every extension of a kept message. A file defining
// a custom option a kept declaration sets is kept whole, with its imports, and
// so is a file of a shared upstream module: neither is the library's own, and
// the upstream ones keep the shape their distributions publish. No file
// declares a service that is not selected.
//
// Bindings are the library's, not one entry's: an aggregated library
// generates every entry into one tree, so a file two entries carry must come
// out the same for both. Each entry is therefore generated from the library's
// narrowing — the closure of every entry's selection at once, where an entry
// naming no Services keeps its whole contract — while its persisted contract
// is its own closure. For a library of one entry the two are the same set.
//
// The entry's Endpoint keeps the producer's digest: it identifies the contract
// the library was generated from, which a selection change does not move. A
// library in which no entry names Services is returned as is.
func narrowEntries(entries []ContractEntry) ([]ContractEntry, error) {
	if !slices.ContainsFunc(entries, func(entry ContractEntry) bool { return selectsServices(&entry) }) {
		return entries, nil
	}
	library := newNarrowing()
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
		if err := library.index(set, entryLabel(&entries[i])); err != nil {
			return nil, err
		}
	}

	selections := make([][]composition.APIContractService, len(entries))
	for i := range entries {
		if sets[i] == nil {
			continue
		}
		if !selectsServices(&entries[i]) {
			library.selectWhole(sets[i])
			continue
		}
		selected, err := selectedServices(&entries[i])
		if err != nil {
			return nil, err
		}
		selections[i] = selected
		library.selectServices(selected)
	}
	if err := library.close(); err != nil {
		return nil, err
	}

	narrowed := slices.Clone(entries)
	for i := range narrowed {
		if selections[i] == nil {
			continue
		}
		entry := &narrowed[i]
		own := newNarrowing()
		if err := own.index(sets[i], entryLabel(entry)); err != nil {
			return nil, err
		}
		own.selectServices(selections[i])
		if err := own.close(); err != nil {
			return nil, err
		}
		contract, err := own.emit(sets[i])
		if err != nil {
			return nil, fmt.Errorf("entry %s: %w", entryLabel(entry), err)
		}
		generated, err := library.emit(sets[i])
		if err != nil {
			return nil, fmt.Errorf("entry %s: %w", entryLabel(entry), err)
		}
		entry.ContractBytes = contract
		entry.generateFrom = generated
		entry.Endpoint.Services = selections[i]
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

// narrowing is the closure of what a set of selected services needs, over
// every file the contracts it indexes carry.
type narrowing struct {
	files        map[string]*descriptorpb.FileDescriptorProto
	origin       map[string]string // file -> the entry it was first indexed from
	upstream     map[string]bool
	types        map[string]declaration // message or enum full name -> its top-level declaration
	messages     map[string]*descriptorpb.DescriptorProto
	services     map[string]string // service full name -> file
	extensions   map[extensionKey]extensionDefinition
	extensionsOf map[string][]extensionDeclaration // extendee full name -> its extensions
	selected     map[string]bool                   // service full names
	kept         map[string]*keptFile
	err          error
}

func newNarrowing() *narrowing {
	return &narrowing{
		files:        map[string]*descriptorpb.FileDescriptorProto{},
		origin:       map[string]string{},
		upstream:     map[string]bool{},
		types:        map[string]declaration{},
		messages:     map[string]*descriptorpb.DescriptorProto{},
		services:     map[string]string{},
		extensions:   map[extensionKey]extensionDefinition{},
		extensionsOf: map[string][]extensionDeclaration{},
		selected:     map[string]bool{},
		kept:         map[string]*keptFile{},
	}
}

// declaration is a top-level message or enum of a file.
type declaration struct {
	file    string
	name    string // its full name
	message *descriptorpb.DescriptorProto
	enum    *descriptorpb.EnumDescriptorProto
}

type extensionKey struct {
	extendee string
	number   int32
}

// extensionDefinition is where an extension is declared and, when its value
// is a message, that message's full name.
type extensionDefinition struct {
	file        string
	messageType string
}

// extensionDeclaration is one extension of a message: a file's top-level
// extension, or one nested in a message (container), which keeps it.
type extensionDeclaration struct {
	file      string
	field     *descriptorpb.FieldDescriptorProto
	container *declaration
}

// keptFile is what a file keeps: everything when whole, otherwise the named
// top-level declarations, and the imports they need.
type keptFile struct {
	whole      bool
	messages   map[string]bool
	enums      map[string]bool
	extensions map[string]bool
	imports    map[string]bool
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

// index adds set's files. A file another entry already carries must be the
// same file: one library generates one copy of it.
func (n *narrowing) index(set *descriptorpb.FileDescriptorSet, entry string) error {
	for _, file := range set.GetFile() {
		name := file.GetName()
		if seen, ok := n.files[name]; ok {
			if !googleproto.Equal(seen, file) {
				return fmt.Errorf("entries %s and %s carry different versions of %s; one library generates one copy of a file", n.origin[name], entry, name)
			}
			continue
		}
		n.files[name] = file
		n.origin[name] = entry
		if _, upstream := matchUpstreamModule(file); upstream {
			n.upstream[name] = true
		}
		pkg := file.GetPackage()
		for _, message := range file.GetMessageType() {
			fullName := qualify(pkg, message.GetName())
			n.indexMessage(fullName, declaration{file: name, name: fullName, message: message}, message)
		}
		for _, enum := range file.GetEnumType() {
			fullName := qualify(pkg, enum.GetName())
			n.types[fullName] = declaration{file: name, name: fullName, enum: enum}
		}
		for _, service := range file.GetService() {
			n.services[qualify(pkg, service.GetName())] = name
		}
		n.indexExtensions(name, nil, file.GetExtension())
	}
	return nil
}

func (n *narrowing) indexMessage(fullName string, top declaration, message *descriptorpb.DescriptorProto) {
	n.types[fullName] = top
	n.messages[fullName] = message
	for _, nested := range message.GetNestedType() {
		n.indexMessage(fullName+"."+nested.GetName(), top, nested)
	}
	for _, enum := range message.GetEnumType() {
		n.types[fullName+"."+enum.GetName()] = top
	}
	n.indexExtensions(top.file, &top, message.GetExtension())
}

func (n *narrowing) indexExtensions(file string, container *declaration, extensions []*descriptorpb.FieldDescriptorProto) {
	for _, extension := range extensions {
		extendee := strings.TrimPrefix(extension.GetExtendee(), ".")
		definition := extensionDefinition{file: file}
		if extension.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
			definition.messageType = strings.TrimPrefix(extension.GetTypeName(), ".")
		}
		n.extensions[extensionKey{extendee, extension.GetNumber()}] = definition
		n.extensionsOf[extendee] = append(n.extensionsOf[extendee], extensionDeclaration{file: file, field: extension, container: container})
	}
}

// selectWhole keeps every file of set whole, with every service it declares.
func (n *narrowing) selectWhole(set *descriptorpb.FileDescriptorSet) {
	for _, file := range set.GetFile() {
		n.keepWhole(file.GetName())
		for _, service := range file.GetService() {
			n.selected[qualify(file.GetPackage(), service.GetName())] = true
		}
	}
}

func (n *narrowing) selectServices(services []composition.APIContractService) {
	for _, service := range services {
		n.selected[service.FullName] = true
	}
}

// close keeps what every selected service needs.
func (n *narrowing) close() error {
	roots := make([]string, 0, len(n.selected))
	for fullName := range n.selected {
		roots = append(roots, fullName)
	}
	sort.Strings(roots)
	for _, fullName := range roots {
		n.keepService(fullName)
	}
	if n.err != nil {
		return fmt.Errorf("cannot narrow the contract to its selected services: %w", n.err)
	}
	return nil
}

// file is name's kept state, created (and its file options visited) the first
// time anything of it is kept.
func (n *narrowing) file(name string) *keptFile {
	if kept, ok := n.kept[name]; ok {
		return kept
	}
	kept := &keptFile{messages: map[string]bool{}, enums: map[string]bool{}, extensions: map[string]bool{}, imports: map[string]bool{}}
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
	file := n.files[name]
	for _, dependency := range file.GetDependency() {
		n.keepWhole(dependency)
	}
	if n.upstream[name] {
		return
	}
	for _, message := range file.GetMessageType() {
		n.followExtensions(qualify(file.GetPackage(), message.GetName()), message)
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
	n.keep(decl)
}

func (n *narrowing) keep(decl declaration) {
	kept := n.file(decl.file)
	switch {
	case kept.whole:
	case n.upstream[decl.file]:
		n.keepWhole(decl.file)
	case decl.message != nil && !kept.messages[decl.message.GetName()]:
		kept.messages[decl.message.GetName()] = true
		n.visitMessage(decl.file, decl.message)
		n.followExtensions(decl.name, decl.message)
	case decl.enum != nil && !kept.enums[decl.enum.GetName()]:
		kept.enums[decl.enum.GetName()] = true
		n.visitEnum(decl.file, decl.enum)
	}
}

// followExtensions keeps every extension of a kept message (and of the types
// nested in it), wherever it is declared: the message's readers rely on them.
func (n *narrowing) followExtensions(fullName string, message *descriptorpb.DescriptorProto) {
	for _, extension := range n.extensionsOf[fullName] {
		if extension.container != nil {
			n.keep(*extension.container)
			continue
		}
		kept := n.file(extension.file)
		if kept.whole || kept.extensions[extension.field.GetName()] {
			continue
		}
		kept.extensions[extension.field.GetName()] = true
		n.visitField(extension.file, extension.field)
		n.useType(extension.file, extension.field.GetExtendee())
	}
	for _, nested := range message.GetNestedType() {
		n.followExtensions(fullName+"."+nested.GetName(), nested)
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
	for _, extensionRange := range message.GetExtensionRange() {
		n.visitOptions(file, extensionRange.GetOptions())
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
// options, however deep in a message-valued option it is set.
func (n *narrowing) visitOptions(from string, options googleproto.Message) {
	if options == nil || !options.ProtoReflect().IsValid() {
		return
	}
	name := string(options.ProtoReflect().Descriptor().FullName())
	data, err := googleproto.MarshalOptions{Deterministic: true}.Marshal(options)
	if err != nil {
		n.fail("%s: cannot read %s: %w", from, name, err)
		return
	}
	n.visitValue(from, name, data)
}

// visitValue walks the encoded value of message: a field the message does not
// declare is an extension, whose defining file is kept whole, and a
// message-valued field (declared or extension) is walked in turn.
func (n *narrowing) visitValue(from, message string, data []byte) {
	for len(data) > 0 {
		number, kind, length := protowire.ConsumeTag(data)
		if length < 0 {
			n.fail("%s: malformed %s", from, message)
			return
		}
		data = data[length:]
		if length = protowire.ConsumeFieldValue(number, kind, data); length < 0 {
			n.fail("%s: malformed %s", from, message)
			return
		}
		value := data[:length]
		data = data[length:]
		declared, messageType, known := n.fieldOf(message, number)
		if !known {
			n.fail("%s sets an option of type %s, which neither the contract nor the protobuf runtime describes", from, message)
			return
		}
		if !declared {
			definition, ok := n.extensions[extensionKey{message, int32(number)}]
			if !ok {
				n.fail("%s sets option %d of %s, which no file of the contract defines", from, number, message)
				continue
			}
			n.require(from, definition.file)
			n.keepWhole(definition.file)
			messageType = definition.messageType
		}
		if messageType == "" || kind != protowire.BytesType {
			continue
		}
		if inner, length := protowire.ConsumeBytes(value); length >= 0 {
			n.visitValue(from, messageType, inner)
		}
	}
}

// fieldOf is whether message declares field number and, when it is
// message-valued, its message type; known is false when neither the contract
// nor the protobuf runtime describes message.
func (n *narrowing) fieldOf(message string, number protowire.Number) (declared bool, messageType string, known bool) {
	if descriptor, ok := n.messages[message]; ok {
		for _, field := range descriptor.GetField() {
			if field.GetNumber() != int32(number) {
				continue
			}
			if field.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
				messageType = strings.TrimPrefix(field.GetTypeName(), ".")
			}
			return true, messageType, true
		}
		return false, "", true
	}
	found, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(message))
	descriptor, ok := found.(protoreflect.MessageDescriptor)
	if err != nil || !ok {
		return false, "", false
	}
	field := descriptor.Fields().ByNumber(number)
	if field == nil {
		return false, "", true
	}
	if field.Message() != nil {
		messageType = string(field.Message().FullName())
	}
	return true, messageType, true
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
	extensions := keptIndexes(len(file.GetExtension()), func(i int) bool {
		return kept.whole || kept.extensions[file.GetExtension()[i].GetName()]
	})
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
