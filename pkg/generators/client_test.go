package generators

import (
	"bytes"
	"testing"

	"github.com/codefly-dev/core/languages"
	"google.golang.org/protobuf/encoding/protowire"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// TestMarkUpstreamModulesAsImportsForGo pins which files of a persisted
// contract buf is told not to generate for, and which buf module each is
// attributed to: the well-known types, googleapis and protovalidate all ship
// canonical Go packages, so a local copy is at best dead code and at worst a
// second registration of a proto file the consumer's upstream module already
// registered. The module identity is what makes the go_package survive: the Go
// template's managed mode excepts exactly these two modules, so the module's
// own bindings import them from upstream rather than from a path in the
// generated library that no longer exists.
//
// A file living under google/protobuf/ but declaring the module's own package
// is the module's, not a well-known type — a vendored copy of the well-known
// types puts real files at those paths, and it is the package that tells the
// two apart.
func TestMarkUpstreamModulesAsImportsForGo(t *testing.T) {
	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    googleproto.String("google/protobuf/timestamp.proto"),
				Package: googleproto.String("google.protobuf"),
			},
			{
				Name:    googleproto.String("google/api/annotations.proto"),
				Package: googleproto.String("google.api"),
			},
			{
				Name:    googleproto.String("google/rpc/status.proto"),
				Package: googleproto.String("google.rpc"),
			},
			{
				Name:    googleproto.String("buf/validate/validate.proto"),
				Package: googleproto.String("buf.validate"),
			},
			{
				Name:    googleproto.String("buf/validate/priv/private.proto"),
				Package: googleproto.String("buf.validate.priv"),
			},
			{
				Name:    googleproto.String("saas/accounts/v1/accounts.proto"),
				Package: googleproto.String("saas.accounts.v1"),
				Dependency: []string{
					"google/protobuf/timestamp.proto",
					"google/api/annotations.proto",
					"buf/validate/validate.proto",
				},
			},
			{
				Name:    googleproto.String("google/protobuf/accounts_extras.proto"),
				Package: googleproto.String("saas.accounts.v1"),
			},
		},
	}

	image := markSet(t, set, languages.GO)

	want := map[string]string{
		"google/protobuf/timestamp.proto":       "",
		"google/api/annotations.proto":          "buf.build/googleapis/googleapis",
		"google/rpc/status.proto":               "buf.build/googleapis/googleapis",
		"buf/validate/validate.proto":           "buf.build/bufbuild/protovalidate",
		"buf/validate/priv/private.proto":       "buf.build/bufbuild/protovalidate",
		"saas/accounts/v1/accounts.proto":       "-",
		"google/protobuf/accounts_extras.proto": "-",
	}
	if len(image.GetFile()) != len(want) {
		t.Fatalf("marked image has %d files, want %d", len(image.GetFile()), len(want))
	}
	for _, file := range image.GetFile() {
		expected, known := want[file.GetName()]
		if !known {
			t.Fatalf("unexpected file %s in marked image", file.GetName())
		}
		isImport, moduleIdentity := bufImageExtension(t, file)
		if isImport != (expected != "-") {
			t.Errorf("%s: is_import = %v, want %v", file.GetName(), isImport, expected != "-")
		}
		if expected != "-" && moduleIdentity != expected {
			t.Errorf("%s: module identity = %q, want %q", file.GetName(), moduleIdentity, expected)
		}
	}

	own := image.GetFile()[5]
	if len(own.GetDependency()) != 3 {
		t.Errorf("module file lost its dependencies: %v", own.GetDependency())
	}
}

// TestMarkUpstreamModulesAsImportsOnlyMapsGo pins the language gate. Only the
// Go template runs managed mode, and only it lists googleapis and
// protovalidate in go_package_prefix.except; the TypeScript and Python
// bindings import a dependency's own generated output unconditionally, so
// dropping those files there would leave the module's bindings importing a
// file buf never wrote. The well-known types are dropped for every language:
// every runtime ships them.
func TestMarkUpstreamModulesAsImportsOnlyMapsGo(t *testing.T) {
	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    googleproto.String("google/protobuf/timestamp.proto"),
				Package: googleproto.String("google.protobuf"),
			},
			{
				Name:    googleproto.String("buf/validate/validate.proto"),
				Package: googleproto.String("buf.validate"),
			},
			{
				Name:    googleproto.String("google/api/annotations.proto"),
				Package: googleproto.String("google.api"),
			},
		},
	}

	for _, lang := range []languages.Language{languages.TYPESCRIPT, languages.PYTHON} {
		image := markSet(t, set, lang)
		want := map[string]bool{
			"google/protobuf/timestamp.proto": true,
			"buf/validate/validate.proto":     false,
			"google/api/annotations.proto":    false,
		}
		for _, file := range image.GetFile() {
			isImport, _ := bufImageExtension(t, file)
			if isImport != want[file.GetName()] {
				t.Errorf("%s: %s is_import = %v, want %v", lang, file.GetName(), isImport, want[file.GetName()])
			}
		}
	}
}

// TestMarkUpstreamModulesAsImportsWireFormat pins the exact bytes buf reads
// the markers from. Every other assertion in this file is written against the
// same field-number constants the markers are built from, so it would keep
// passing if those drifted from buf.alpha.image.v1's actual schema and buf
// silently went back to generating — and go_package-rewriting — these files;
// this is what catches that.
func TestMarkUpstreamModulesAsImportsWireFormat(t *testing.T) {
	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    googleproto.String("google/protobuf/timestamp.proto"),
				Package: googleproto.String("google.protobuf"),
			},
			{
				Name:    googleproto.String("buf/validate/validate.proto"),
				Package: googleproto.String("buf.validate"),
			},
		},
	}

	image := markSet(t, set, languages.GO)

	// Field 8042, length-delimited (0xd2 0xf6 0x03), wrapping is_import=true
	// (0x08 0x01).
	wellKnown := []byte{0xd2, 0xf6, 0x03, 0x02, 0x08, 0x01}
	if got := []byte(image.GetFile()[0].ProtoReflect().GetUnknown()); !bytes.Equal(got, wellKnown) {
		t.Fatalf("well-known type extension = % x, want % x", got, wellKnown)
	}

	// The same, plus module_info (field 2) wrapping name (field 1) wrapping
	// remote/owner/repository (fields 1, 2, 3).
	var moduleName []byte
	moduleName = append(moduleName, 0x0a, 0x09)
	moduleName = append(moduleName, "buf.build"...)
	moduleName = append(moduleName, 0x12, 0x08)
	moduleName = append(moduleName, "bufbuild"...)
	moduleName = append(moduleName, 0x1a, 0x0d)
	moduleName = append(moduleName, "protovalidate"...)
	moduleInfo := append([]byte{0x0a, byte(len(moduleName))}, moduleName...)
	extension := append([]byte{0x08, 0x01, 0x12, byte(len(moduleInfo))}, moduleInfo...)
	mapped := append([]byte{0xd2, 0xf6, 0x03, byte(len(extension))}, extension...)
	if got := []byte(image.GetFile()[1].ProtoReflect().GetUnknown()); !bytes.Equal(got, mapped) {
		t.Fatalf("protovalidate extension = % x, want % x", got, mapped)
	}
}

func markSet(t *testing.T, set *descriptorpb.FileDescriptorSet, lang languages.Language) *descriptorpb.FileDescriptorSet {
	t.Helper()
	// markUpstreamModulesAsImports mutates the files it marks, so every case
	// starts from its own copy of the fixture.
	data, err := markUpstreamModulesAsImports(googleproto.Clone(set).(*descriptorpb.FileDescriptorSet), lang)
	if err != nil {
		t.Fatalf("markUpstreamModulesAsImports(%s): %v", lang, err)
	}
	var image descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(data, &image); err != nil {
		t.Fatalf("unmarshal marked image: %v", err)
	}
	return &image
}

// bufImageExtension reads back the
// buf.alpha.image.v1.ImageFileExtension buf keeps on a FileDescriptorProto's
// extension field 8042: is_import, which tells buf the file is in the image
// only to resolve imports, and the "remote/owner/repository" identity of the
// buf module it came from, which managed mode's except list matches against.
func bufImageExtension(t *testing.T, file *descriptorpb.FileDescriptorProto) (bool, string) {
	t.Helper()
	extension := bufImageExtensionBytes(t, file)
	var isImport bool
	var moduleIdentity string
	for len(extension) > 0 {
		number, kind, headerLength := protowire.ConsumeTag(extension)
		if headerLength < 0 {
			t.Fatalf("%s: cannot read image file extension tag: %v", file.GetName(), protowire.ParseError(headerLength))
		}
		extension = extension[headerLength:]
		switch {
		case number == bufImageFileIsImportField && kind == protowire.VarintType:
			value, length := protowire.ConsumeVarint(extension)
			if length < 0 {
				t.Fatalf("%s: cannot read is_import: %v", file.GetName(), protowire.ParseError(length))
			}
			isImport = value == 1
			extension = extension[length:]
		case number == bufImageFileModuleInfoField && kind == protowire.BytesType:
			value, length := protowire.ConsumeBytes(extension)
			if length < 0 {
				t.Fatalf("%s: cannot read module_info: %v", file.GetName(), protowire.ParseError(length))
			}
			moduleIdentity = bufModuleIdentity(t, file.GetName(), value)
			extension = extension[length:]
		default:
			skipped := protowire.ConsumeFieldValue(number, kind, extension)
			if skipped < 0 {
				t.Fatalf("%s: cannot skip image file extension field %d: %v", file.GetName(), number, protowire.ParseError(skipped))
			}
			extension = extension[skipped:]
		}
	}
	return isImport, moduleIdentity
}

// bufImageExtensionBytes returns the raw ImageFileExtension bytes carried on
// file, or nil when it carries none.
func bufImageExtensionBytes(t *testing.T, file *descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	unknown := []byte(file.ProtoReflect().GetUnknown())
	for len(unknown) > 0 {
		number, kind, headerLength := protowire.ConsumeTag(unknown)
		if headerLength < 0 {
			t.Fatalf("%s: cannot read unknown field tag: %v", file.GetName(), protowire.ParseError(headerLength))
		}
		unknown = unknown[headerLength:]
		if number != bufImageFileExtensionField || kind != protowire.BytesType {
			skipped := protowire.ConsumeFieldValue(number, kind, unknown)
			if skipped < 0 {
				t.Fatalf("%s: cannot skip unknown field %d: %v", file.GetName(), number, protowire.ParseError(skipped))
			}
			unknown = unknown[skipped:]
			continue
		}
		extension, valueLength := protowire.ConsumeBytes(unknown)
		if valueLength < 0 {
			t.Fatalf("%s: cannot read image file extension: %v", file.GetName(), protowire.ParseError(valueLength))
		}
		return extension
	}
	return nil
}

// bufModuleIdentity renders a buf.alpha.image.v1.ModuleInfo's name as buf
// writes it in managed mode's except list: "remote/owner/repository".
func bufModuleIdentity(t *testing.T, fileName string, moduleInfo []byte) string {
	t.Helper()
	number, kind, headerLength := protowire.ConsumeTag(moduleInfo)
	if headerLength < 0 || number != bufModuleInfoNameField || kind != protowire.BytesType {
		t.Fatalf("%s: module_info does not start with a name field: % x", fileName, moduleInfo)
	}
	name, valueLength := protowire.ConsumeBytes(moduleInfo[headerLength:])
	if valueLength < 0 {
		t.Fatalf("%s: cannot read module name: %v", fileName, protowire.ParseError(valueLength))
	}
	segments := map[protowire.Number]string{}
	for len(name) > 0 {
		fieldNumber, fieldKind, tagLength := protowire.ConsumeTag(name)
		if tagLength < 0 || fieldKind != protowire.BytesType {
			t.Fatalf("%s: cannot read module name field: % x", fileName, name)
		}
		value, length := protowire.ConsumeString(name[tagLength:])
		if length < 0 {
			t.Fatalf("%s: cannot read module name field %d: %v", fileName, fieldNumber, protowire.ParseError(length))
		}
		segments[fieldNumber] = value
		name = name[tagLength+length:]
	}
	return segments[bufModuleNameRemoteField] + "/" + segments[bufModuleNameOwnerField] + "/" + segments[bufModuleNameRepositoryField]
}
