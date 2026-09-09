package generators

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// TestMarkWellKnownTypesAsImports pins which files of a persisted contract buf
// is told not to generate for: the well-known types only. Marking the module's
// own files would generate nothing at all, and marking the other shared imports
// (google/api, buf.validate) would leave the module's bindings importing a
// package that no longer exists, since managed mode rewrites their go_package
// to the generated library's own path.
func TestMarkWellKnownTypesAsImports(t *testing.T) {
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
				Name:       googleproto.String("saas/accounts/v1/accounts.proto"),
				Package:    googleproto.String("saas.accounts.v1"),
				Dependency: []string{"google/protobuf/timestamp.proto", "google/api/annotations.proto"},
			},
		},
	}

	data, err := markWellKnownTypesAsImports(set)
	if err != nil {
		t.Fatalf("markWellKnownTypesAsImports: %v", err)
	}

	var image descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(data, &image); err != nil {
		t.Fatalf("unmarshal marked image: %v", err)
	}

	want := map[string]bool{
		"google/protobuf/timestamp.proto": true,
		"google/api/annotations.proto":    false,
		"saas/accounts/v1/accounts.proto": false,
	}
	if len(image.GetFile()) != len(want) {
		t.Fatalf("marked image has %d files, want %d", len(image.GetFile()), len(want))
	}
	for _, file := range image.GetFile() {
		expected, known := want[file.GetName()]
		if !known {
			t.Fatalf("unexpected file %s in marked image", file.GetName())
		}
		if got := bufIsImport(t, file); got != expected {
			t.Errorf("%s: is_import = %v, want %v", file.GetName(), got, expected)
		}
	}

	own := image.GetFile()[2]
	if len(own.GetDependency()) != 2 {
		t.Errorf("module file lost its dependencies: %v", own.GetDependency())
	}
}

// bufIsImport reports whether file carries
// buf.alpha.image.v1.ImageFileExtension.is_import, which buf reads off a
// FileDescriptorProto's extension field 8042 to decide a file is in the image
// only to resolve imports.
func bufIsImport(t *testing.T, file *descriptorpb.FileDescriptorProto) bool {
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
		unknown = unknown[valueLength:]
		fieldNumber, fieldKind, tagLength := protowire.ConsumeTag(extension)
		if tagLength < 0 {
			t.Fatalf("%s: cannot read image file extension tag: %v", file.GetName(), protowire.ParseError(tagLength))
		}
		if fieldNumber != bufImageFileIsImportField || fieldKind != protowire.VarintType {
			continue
		}
		isImport, importLength := protowire.ConsumeVarint(extension[tagLength:])
		if importLength < 0 {
			t.Fatalf("%s: cannot read is_import: %v", file.GetName(), protowire.ParseError(importLength))
		}
		return isImport == 1
	}
	return false
}
