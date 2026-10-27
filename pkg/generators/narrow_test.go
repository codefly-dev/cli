package generators

import (
	"bytes"
	"slices"
	"sort"
	"strings"
	"testing"

	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	"google.golang.org/protobuf/encoding/protowire"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const scopeOption = 50001

func messageField(name string, number int32, typeName string) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     googleproto.String(name),
		Number:   googleproto.Int32(number),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: googleproto.String(typeName),
	}
}

func enumField(name string, number int32, typeName string) *descriptorpb.FieldDescriptorProto {
	field := messageField(name, number, typeName)
	field.Type = descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
	return field
}

func message(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
	return &descriptorpb.DescriptorProto{Name: googleproto.String(name), Field: fields}
}

func enum(name string) *descriptorpb.EnumDescriptorProto {
	return &descriptorpb.EnumDescriptorProto{
		Name:  googleproto.String(name),
		Value: []*descriptorpb.EnumValueDescriptorProto{{Name: googleproto.String(strings.ToUpper(name) + "_UNSPECIFIED"), Number: googleproto.Int32(0)}},
	}
}

func service(name string, methods ...*descriptorpb.MethodDescriptorProto) *descriptorpb.ServiceDescriptorProto {
	return &descriptorpb.ServiceDescriptorProto{Name: googleproto.String(name), Method: methods}
}

func method(name, input, output string) *descriptorpb.MethodDescriptorProto {
	return &descriptorpb.MethodDescriptorProto{Name: googleproto.String(name), InputType: googleproto.String(input), OutputType: googleproto.String(output)}
}

func protoFile(name string, deps []string, messages []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto, services ...*descriptorpb.ServiceDescriptorProto) *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:        googleproto.String(name),
		Package:     googleproto.String("acme.v1"),
		Syntax:      googleproto.String("proto3"),
		Dependency:  deps,
		MessageType: messages,
		EnumType:    enums,
		Service:     services,
	}
}

func leadingComment(path []int32, text string) *descriptorpb.SourceCodeInfo_Location {
	return &descriptorpb.SourceCodeInfo_Location{Path: path, Span: []int32{0, 0, 1}, LeadingComments: googleproto.String(text)}
}

// narrowFixture is a contract of two services, Wanted and Other, over a
// shared file that also declares a service and declarations neither needs.
// Wanted sets a custom method option defined in its own file, names a
// well-known type, an enum, a nested type of a shared message, and a nested
// type of its own request; Other is the only one importing empty.proto.
func narrowFixture(t *testing.T) []byte {
	t.Helper()
	holder := message("Holder")
	holder.NestedType = []*descriptorpb.DescriptorProto{message("Nested")}
	common := protoFile("acme/v1/common.proto", nil,
		[]*descriptorpb.DescriptorProto{
			message("Unused"),
			message("Shared", messageField("leaf", 1, ".acme.v1.Leaf")),
			message("Leaf"),
			holder,
		},
		[]*descriptorpb.EnumDescriptorProto{enum("UnusedEnum"), enum("Color")},
		service("CommonService", method("Ping", ".acme.v1.Unused", ".acme.v1.Unused")),
	)
	common.SourceCodeInfo = &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{
		leadingComment([]int32{4, 0}, " Unused doc\n"),
		leadingComment([]int32{4, 2}, " Leaf doc\n"),
		leadingComment([]int32{5, 1}, " Color doc\n"),
		leadingComment([]int32{6, 0}, " CommonService doc\n"),
	}}

	options := protoFile("acme/v1/options.proto", []string{"google/protobuf/descriptor.proto"}, nil, nil)
	options.Extension = []*descriptorpb.FieldDescriptorProto{{
		Name:     googleproto.String("scope"),
		Number:   googleproto.Int32(scopeOption),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		Extendee: googleproto.String(".google.protobuf.MethodOptions"),
	}}

	request := message("WantedRequest",
		messageField("shared", 1, ".acme.v1.Shared"),
		enumField("color", 2, ".acme.v1.Color"),
		messageField("at", 3, ".google.protobuf.Timestamp"),
		messageField("inner", 4, ".acme.v1.WantedRequest.Inner"),
	)
	request.NestedType = []*descriptorpb.DescriptorProto{message("Inner")}
	get := method("Get", ".acme.v1.WantedRequest", ".acme.v1.WantedResponse")
	get.Options = &descriptorpb.MethodOptions{}
	get.Options.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, scopeOption, protowire.BytesType), "read"))
	wanted := protoFile("acme/v1/wanted.proto",
		[]string{"acme/v1/common.proto", "acme/v1/options.proto", "google/protobuf/timestamp.proto"},
		[]*descriptorpb.DescriptorProto{request, message("WantedResponse", messageField("nested", 1, ".acme.v1.Holder.Nested"))},
		nil,
		service("Wanted", get),
	)

	other := protoFile("acme/v1/other.proto",
		[]string{"acme/v1/common.proto", "google/protobuf/empty.proto"},
		[]*descriptorpb.DescriptorProto{message("OtherRequest", messageField("unused", 1, ".acme.v1.Unused"))},
		nil,
		service("Other", method("Do", ".acme.v1.OtherRequest", ".google.protobuf.Empty")),
	)

	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(descriptorpb.File_google_protobuf_descriptor_proto),
		protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto),
		protodesc.ToFileDescriptorProto(emptypb.File_google_protobuf_empty_proto),
		common, options, wanted, other,
	}}
	if _, err := protodesc.NewFiles(set); err != nil {
		t.Fatalf("fixture does not resolve: %v", err)
	}
	data, err := googleproto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func narrowEntry(contract []byte, services ...string) ContractEntry {
	var set descriptorpb.FileDescriptorSet
	_ = googleproto.Unmarshal(contract, &set)
	return ContractEntry{
		Endpoint: composition.APIContractEndpoint{
			Service:  "api",
			Endpoint: "connect",
			Kind:     composition.APIContractKindProtobuf,
			Package:  "acme.v1",
			Digest:   composition.APIContractDigest(contract),
			Services: composition.ProtobufServices(&set, "acme.v1"),
		},
		ContractBytes: contract,
		ModuleName:    "acme",
		ProtoSources:  []coreproto.Source{{Path: "acme/v1/wanted.proto"}},
		Services:      services,
	}
}

func parseNarrowed(t *testing.T, data []byte) map[string]*descriptorpb.FileDescriptorProto {
	t.Helper()
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	if _, err := protodesc.NewFiles(&set); err != nil {
		t.Fatalf("narrowed contract does not resolve: %v", err)
	}
	files := map[string]*descriptorpb.FileDescriptorProto{}
	for _, file := range set.GetFile() {
		files[file.GetName()] = file
	}
	return files
}

func declared(file *descriptorpb.FileDescriptorProto) (messages, enums, services []string) {
	for _, m := range file.GetMessageType() {
		messages = append(messages, m.GetName())
	}
	for _, e := range file.GetEnumType() {
		enums = append(enums, e.GetName())
	}
	for _, s := range file.GetService() {
		services = append(services, s.GetName())
	}
	return messages, enums, services
}

func keys(files map[string]*descriptorpb.FileDescriptorProto) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// One selected service keeps exactly its methods' messages, what they name,
// the file defining the option it sets and the well-known types it uses; the
// other service, its file, its import and every declaration only it reaches
// are gone.
func TestNarrowEntriesKeepsOnlyTheSelectedServiceAndWhatItNeeds(t *testing.T) {
	contract := narrowFixture(t)
	narrowed, err := narrowEntries([]ContractEntry{narrowEntry(contract, "Wanted")})
	if err != nil {
		t.Fatal(err)
	}
	entry := narrowed[0]
	files := parseNarrowed(t, entry.ContractBytes)

	wantFiles := []string{"acme/v1/common.proto", "acme/v1/options.proto", "acme/v1/wanted.proto", "google/protobuf/descriptor.proto", "google/protobuf/timestamp.proto"}
	if got := keys(files); !slices.Equal(got, wantFiles) {
		t.Fatalf("files = %v, want %v", got, wantFiles)
	}
	var services []string
	for _, file := range files {
		_, _, s := declared(file)
		services = append(services, s...)
	}
	if !slices.Equal(services, []string{"Wanted"}) {
		t.Fatalf("services = %v, want only Wanted", services)
	}
	messages, enums, _ := declared(files["acme/v1/common.proto"])
	if want := []string{"Shared", "Leaf", "Holder"}; !slices.Equal(messages, want) {
		t.Errorf("common.proto messages = %v, want %v", messages, want)
	}
	if want := []string{"Color"}; !slices.Equal(enums, want) {
		t.Errorf("common.proto enums = %v, want %v", enums, want)
	}
	if got := files["acme/v1/common.proto"].GetMessageType()[2].GetNestedType(); len(got) != 1 {
		t.Errorf("Holder lost its nested types: %v", got)
	}
	if got, want := files["acme/v1/wanted.proto"].GetDependency(), []string{"acme/v1/common.proto", "acme/v1/options.proto", "google/protobuf/timestamp.proto"}; !slices.Equal(got, want) {
		t.Errorf("wanted.proto imports = %v, want %v", got, want)
	}
	if len(files["acme/v1/options.proto"].GetExtension()) != 1 {
		t.Error("the file defining the option Wanted sets lost its extension")
	}
	if got := len(files["google/protobuf/descriptor.proto"].GetMessageType()); got != len(protodesc.ToFileDescriptorProto(descriptorpb.File_google_protobuf_descriptor_proto).GetMessageType()) {
		t.Errorf("descriptor.proto was narrowed to %d messages; an upstream file is kept whole", got)
	}

	comments := map[string]string{}
	common := files["acme/v1/common.proto"]
	for _, location := range common.GetSourceCodeInfo().GetLocation() {
		path := location.GetPath()
		switch path[0] {
		case 4:
			comments[common.GetMessageType()[path[1]].GetName()] = location.GetLeadingComments()
		case 5:
			comments[common.GetEnumType()[path[1]].GetName()] = location.GetLeadingComments()
		default:
			t.Errorf("a comment of a dropped declaration survived: %v %q", path, location.GetLeadingComments())
		}
	}
	if want := map[string]string{"Leaf": " Leaf doc\n", "Color": " Color doc\n"}; len(comments) != len(want) || comments["Leaf"] != want["Leaf"] || comments["Color"] != want["Color"] {
		t.Errorf("comments = %v, want %v", comments, want)
	}

	if len(entry.Endpoint.Services) != 1 || entry.Endpoint.Services[0].FullName != "acme.v1.Wanted" {
		t.Errorf("catalog services = %+v, want only acme.v1.Wanted", entry.Endpoint.Services)
	}
	if entry.Endpoint.Digest != composition.APIContractDigest(contract) {
		t.Error("the entry no longer records the producer's contract digest")
	}
	if !bytes.Equal(entry.generateFrom, entry.ContractBytes) {
		t.Error("a library of one entry is not generated from the contract it persists")
	}
	if entry.ProtoSources != nil {
		t.Error("a narrowed entry still generates from the whole contract's sources")
	}
}

// Narrowing is a function of the contract: the same input gives the same
// bytes, and narrowing a narrowed contract changes nothing.
func TestNarrowEntriesIsStable(t *testing.T) {
	contract := narrowFixture(t)
	first, err := narrowEntries([]ContractEntry{narrowEntry(contract, "Wanted")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := narrowEntries([]ContractEntry{narrowEntry(contract, "Wanted")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first[0].ContractBytes, second[0].ContractBytes) {
		t.Fatal("two narrowings of one contract differ")
	}
	again, err := narrowEntries([]ContractEntry{narrowEntry(first[0].ContractBytes, "Wanted")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first[0].ContractBytes, again[0].ContractBytes) {
		t.Fatal("narrowing a narrowed contract changed it")
	}
}

func TestNarrowEntriesRefusesAServiceTheContractDoesNotDeclare(t *testing.T) {
	_, err := narrowEntries([]ContractEntry{narrowEntry(narrowFixture(t), "Wanted", "Missing")})
	if err == nil || !strings.Contains(err.Error(), `"Missing"`) || !strings.Contains(err.Error(), "Other, Wanted") {
		t.Fatalf("err = %v, want it to name Missing and the services declared", err)
	}
}

func TestNarrowEntriesLeavesALibrarySelectingNothingAsIs(t *testing.T) {
	contract := narrowFixture(t)
	entries := []ContractEntry{narrowEntry(contract)}
	narrowed, err := narrowEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(narrowed[0].ContractBytes, contract) || len(narrowed[0].Endpoint.Services) != len(entries[0].Endpoint.Services) || narrowed[0].ProtoSources == nil {
		t.Fatal("an entry naming no services was changed")
	}
}

// Entries of one library are generated into one tree, so a file two entries
// carry is generated the same for both, holding what either needs; each
// entry's persisted contract still declares its own selection alone, and
// records the producer's digest, which neither selection moves.
func TestNarrowEntriesNarrowsAnAggregatedLibraryAsOne(t *testing.T) {
	contract := narrowFixture(t)
	narrowed, err := narrowEntries([]ContractEntry{narrowEntry(contract, "Wanted"), narrowEntry(contract, "Other")})
	if err != nil {
		t.Fatal(err)
	}
	wanted, other := parseNarrowed(t, narrowed[0].generateFrom), parseNarrowed(t, narrowed[1].generateFrom)
	if !googleproto.Equal(wanted["acme/v1/common.proto"], other["acme/v1/common.proto"]) {
		t.Fatal("the shared file is generated differently for the two entries")
	}
	messages, _, services := declared(wanted["acme/v1/common.proto"])
	if !slices.Equal(messages, []string{"Unused", "Shared", "Leaf", "Holder"}) || len(services) != 0 {
		t.Fatalf("common.proto = %v %v, want what Wanted and Other need and no service", messages, services)
	}

	alone, err := narrowEntries([]ContractEntry{narrowEntry(contract, "Wanted")})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"Wanted", "Other"} {
		entry := narrowed[i]
		var services []string
		for _, file := range parseNarrowed(t, entry.ContractBytes) {
			_, _, s := declared(file)
			services = append(services, s...)
		}
		if !slices.Equal(services, []string{want}) || entry.Endpoint.Services[0].Name != want {
			t.Errorf("entry %d persists services %v with catalog %v, want only %s", i, services, entry.Endpoint.Services, want)
		}
		if entry.Endpoint.Digest != composition.APIContractDigest(contract) {
			t.Errorf("entry %d does not record the producer's digest", i)
		}
	}
	if !bytes.Equal(narrowed[0].ContractBytes, alone[0].ContractBytes) {
		t.Error("adding an entry changed another entry's persisted contract")
	}
}

func TestNarrowEntriesRefusesTwoVersionsOfOneFile(t *testing.T) {
	contract := narrowFixture(t)
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(contract, &set); err != nil {
		t.Fatal(err)
	}
	for _, file := range set.GetFile() {
		if file.GetName() == "acme/v1/common.proto" {
			leaf := file.GetMessageType()[2]
			leaf.Field = append(leaf.Field, messageField("extra", 9, ".acme.v1.Shared"))
		}
	}
	changed, err := googleproto.Marshal(&set)
	if err != nil {
		t.Fatal(err)
	}
	other := narrowEntry(changed, "Other")
	other.Endpoint.Endpoint = "connect-other"
	_, err = narrowEntries([]ContractEntry{narrowEntry(contract, "Wanted"), other})
	if err == nil || !strings.Contains(err.Error(), "acme/v1/common.proto") || !strings.Contains(err.Error(), "acme/api/connect-other") {
		t.Fatalf("err = %v, want a refusal naming the file and both entries", err)
	}
}

func proto2File(name string, deps []string, messages ...*descriptorpb.DescriptorProto) *descriptorpb.FileDescriptorProto {
	file := protoFile(name, deps, messages, nil)
	file.Syntax = googleproto.String("proto2")
	return file
}

func stringExtension(name string, number int32, extendee string) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     googleproto.String(name),
		Number:   googleproto.Int32(number),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		Extendee: googleproto.String(extendee),
	}
}

func extensionRange(start, end int32) *descriptorpb.DescriptorProto_ExtensionRange {
	return &descriptorpb.DescriptorProto_ExtensionRange{Start: googleproto.Int32(start), End: googleproto.Int32(end)}
}

// TestNarrowEntriesFollowsExtensionsAndNestedOptions covers what a proto2
// contract adds: an option set on an extension range, an extension of a kept
// message declared in another file, and an extension set inside a
// message-valued option. Each defining file is kept; a declaration of those
// files nothing reaches is not.
func TestNarrowEntriesFollowsExtensionsAndNestedOptions(t *testing.T) {
	rangeOption := proto2File("acme/v1/rangeopt.proto", []string{"google/protobuf/descriptor.proto"})
	rangeOption.Extension = []*descriptorpb.FieldDescriptorProto{stringExtension("range_note", 50002, ".google.protobuf.ExtensionRangeOptions")}

	carrier := message("P2", messageField("self", 1, ".acme.v1.P2"))
	carrier.Field[0].Label = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	carrier.ExtensionRange = []*descriptorpb.DescriptorProto_ExtensionRange{extensionRange(100, 201)}
	carrier.ExtensionRange[0].Options = &descriptorpb.ExtensionRangeOptions{}
	carrier.ExtensionRange[0].Options.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 50002, protowire.BytesType), "note"))
	p2 := proto2File("acme/v1/p2.proto", []string{"acme/v1/rangeopt.proto"}, carrier)

	more := messageField("more", 100, ".acme.v1.Extra")
	more.Extendee = googleproto.String(".acme.v1.P2")
	extending := proto2File("acme/v1/ext.proto", []string{"acme/v1/p2.proto"}, message("Extra"), message("Unrelated"))
	extending.Extension = []*descriptorpb.FieldDescriptorProto{more}

	rule := message("Rule")
	rule.Field = []*descriptorpb.FieldDescriptorProto{stringExtension("name", 1, "")}
	rule.Field[0].Extendee = nil
	rule.ExtensionRange = []*descriptorpb.DescriptorProto_ExtensionRange{extensionRange(10, 21)}
	ruleOption := messageField("rule", 50003, ".acme.v1.Rule")
	ruleOption.Extendee = googleproto.String(".google.protobuf.MethodOptions")
	optionMessage := proto2File("acme/v1/optmsg.proto", []string{"google/protobuf/descriptor.proto"}, rule)
	optionMessage.Extension = []*descriptorpb.FieldDescriptorProto{ruleOption}
	ruleExtension := proto2File("acme/v1/ruleext.proto", []string{"acme/v1/optmsg.proto"})
	ruleExtension.Extension = []*descriptorpb.FieldDescriptorProto{stringExtension("tag", 10, ".acme.v1.Rule")}

	value := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "n")
	value = protowire.AppendString(protowire.AppendTag(value, 10, protowire.BytesType), "t")
	do := method("Do", ".acme.v1.Req", ".acme.v1.Req")
	do.Options = &descriptorpb.MethodOptions{}
	do.Options.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 50003, protowire.BytesType), value))
	service := proto2File("acme/v1/svc.proto", []string{"acme/v1/p2.proto", "acme/v1/optmsg.proto", "acme/v1/ruleext.proto"}, message("Req", messageField("p", 1, ".acme.v1.P2")))
	service.Service = []*descriptorpb.ServiceDescriptorProto{{Name: googleproto.String("S"), Method: []*descriptorpb.MethodDescriptorProto{do}}}

	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(descriptorpb.File_google_protobuf_descriptor_proto),
		rangeOption, p2, extending, optionMessage, ruleExtension, service,
	}}
	if _, err := protodesc.NewFiles(set); err != nil {
		t.Fatalf("fixture does not resolve: %v", err)
	}
	contract, err := googleproto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	narrowed, err := narrowEntries([]ContractEntry{narrowEntry(contract, "S")})
	if err != nil {
		t.Fatal(err)
	}
	files := parseNarrowed(t, narrowed[0].ContractBytes)
	want := []string{"acme/v1/ext.proto", "acme/v1/optmsg.proto", "acme/v1/p2.proto", "acme/v1/rangeopt.proto", "acme/v1/ruleext.proto", "acme/v1/svc.proto", "google/protobuf/descriptor.proto"}
	if got := keys(files); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := files["acme/v1/p2.proto"].GetDependency(); !slices.Equal(got, []string{"acme/v1/rangeopt.proto"}) {
		t.Errorf("p2.proto imports %v, want the file defining its extension-range option", got)
	}
	if got := files["acme/v1/svc.proto"].GetDependency(); !slices.Contains(got, "acme/v1/ruleext.proto") {
		t.Errorf("svc.proto imports %v, want the file defining the extension set inside its rule option", got)
	}
	ext := files["acme/v1/ext.proto"]
	if messages, _, _ := declared(ext); !slices.Equal(messages, []string{"Extra"}) || len(ext.GetExtension()) != 1 {
		t.Errorf("ext.proto keeps %v and %d extensions, want Extra and the extension of P2", messages, len(ext.GetExtension()))
	}
}

// A multi-package contract can declare the same short service name twice.
// Selecting one must never include the other package's service or messages.
func TestNarrowEntriesSelectsQualifiedServiceIdentity(t *testing.T) {
	var files []*descriptorpb.FileDescriptorProto
	for _, pkg := range []string{"acme.v1", "other.v1"} {
		file := protoFile(pkg+"/api.proto", nil,
			[]*descriptorpb.DescriptorProto{message("Request")}, nil,
			service("DirectoryService", method("List", "."+pkg+".Request", "."+pkg+".Request")))
		file.Package = googleproto.String(pkg)
		files = append(files, file)
	}
	set := &descriptorpb.FileDescriptorSet{File: files}
	data, err := googleproto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	entry := narrowEntry(data, "acme.v1.DirectoryService")
	entry.Endpoint.Package = ""
	entry.Endpoint.Services = composition.ProtobufServices(set, "")
	narrowed, err := narrowEntries([]ContractEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	got := parseNarrowed(t, narrowed[0].ContractBytes)
	if len(got) != 1 || got["acme.v1/api.proto"] == nil {
		t.Fatalf("qualified selection includes wrong files: %v", got)
	}
	if !slices.Equal(narrowed[0].Services, []string{"DirectoryService"}) {
		t.Fatalf("companion selection = %v", narrowed[0].Services)
	}
	entry.Services = []string{"DirectoryService"}
	_, err = narrowEntries([]ContractEntry{entry})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "acme.v1.DirectoryService") || !strings.Contains(err.Error(), "other.v1.DirectoryService") {
		t.Fatalf("ambiguous selection error = %v", err)
	}
}
